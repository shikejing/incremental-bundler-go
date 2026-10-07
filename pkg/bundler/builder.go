package bundler

import (
	"bytes"
	"context"
	"fmt"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// Config configures a build.
type Config struct {
	Source    Source
	CacheDir  string
	OutputDir string
	Pipeline  *Pipeline
	// Entries are bundle entry module ids. Empty means every module is an
	// entry. Deterministic regardless of the order given here.
	Entries []string
	// Mode selects a single artifact or one artifact per entry.
	Mode OutputMode
	// MissingPolicy: "error" (default) or "stub".
	MissingPolicy string
	// Workers bounds concurrent module transforms (0 = GOMAXPROCS).
	Workers int
}

// Report describes a completed build in an explainable way.
type Report struct {
	Modules     int
	Transformed int
	CacheHits   int
	Cycles      []Cycle
	Diagnostics []Diagnostic
	Artifacts   []Artifact
	ReleaseDir  string
	CacheReused bool
}

// Builder orchestrates resolve -> transform -> synthesize -> cache/publish.
type Builder struct{ cfg Config }

func New(cfg Config) *Builder { return &Builder{cfg: cfg} }

// Build runs a full or incremental build. Byte-identical inputs and config
// always yield byte-identical artifacts, whether or not the cache is warm.
func (b *Builder) Build(ctx context.Context) (*Report, error) {
	cfg := b.cfg
	if cfg.Source == nil {
		return nil, fmt.Errorf("builder: Source is required")
	}
	if cfg.CacheDir == "" {
		cfg.CacheDir = ".bundler/cache"
	}
	if cfg.OutputDir == "" {
		cfg.OutputDir = ".bundler/dist"
	}
	if cfg.Pipeline == nil {
		cfg.Pipeline = NewPipeline()
	}
	if cfg.MissingPolicy == "" {
		cfg.MissingPolicy = "error"
	}
	workers := cfg.Workers
	if workers <= 0 {
		workers = runtime.GOMAXPROCS(0)
	}

	// 1. Read sources: adds, deletes and renames are all reflected because
	//    the workspace is rescanned on every build.
	files, err := cfg.Source.ReadAll(ctx)
	if err != nil {
		return nil, fmt.Errorf("read sources: %w", err)
	}

	// 2. Resolve the module graph (duplicates / missing / cycles).
	g, diags, err := Resolve(files, ResolveOptions{OnMissing: cfg.MissingPolicy})
	if err != nil {
		return nil, err
	}

	// 3. Open persistent cache; corruption/incompatibility is quarantined
	//    automatically and the build proceeds as a full rebuild.
	cache, err := OpenCache(ctx, cfg.CacheDir)
	if err != nil {
		return nil, err
	}
	defer cache.Close()
	if cache.WasQuarantined() {
		diags = append(diags, Diagnostic{
			Severity: SeverityWarning,
			Kind:     "cache-recovered",
			Message:  "cache index was corrupt or incompatible; quarantined and performing full rebuild",
		})
	}

	// 4. Resolve entries deterministically.
	entries := append([]string(nil), cfg.Entries...)
	if len(entries) == 0 {
		for id := range g.Modules {
			entries = append(entries, id)
		}
	}
	entries = uniqueSorted(entries)
	for _, e := range entries {
		if _, ok := g.Modules[e]; !ok {
			return nil, fmt.Errorf("entry module %q not found in workspace", e)
		}
	}

	// 5. Plan cache keys. SCC fingerprints make every upstream content
	//    change invalidate all downstream modules; cycles stay stable.
	pipelineID := cfg.Pipeline.Hash()
	sccHash := map[int]string{}
	keys := make(map[string]string, len(g.Modules))
	for _, id := range g.Order {
		keys[id] = computeKey(g, id, pipelineID, sccHash)
	}

	// 6. Transform modules concurrently with bounded workers. Slow modules
	//    never block unrelated ones; per-key singleflight deduplicates work
	//    across builds running concurrently in this process.
	workers = minInt(workers, len(g.Order))
	if workers < 1 {
		workers = 1
	}
	sem := make(chan struct{}, workers)
	bctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		wg         sync.WaitGroup
		mu         sync.Mutex
		outputs    = map[string][]byte{}
		hits       int
		misses     int
		moduleErrs []error
	)

	for _, id := range g.Order {
		select {
		case sem <- struct{}{}:
		case <-bctx.Done():
		}
		if bctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			defer func() { <-sem }()
			content, hit, ferr := b.transformOne(bctx, cache, g, id, keys[id], pipelineID, sccHash)
			if ferr != nil {
				mu.Lock()
				moduleErrs = append(moduleErrs, ferr)
				mu.Unlock()
				cancel()
				return
			}
			mu.Lock()
			outputs[id] = content
			if hit {
				hits++
			} else {
				misses++
			}
			mu.Unlock()
		}(id)
	}
	wg.Wait()

	if len(moduleErrs) > 0 {
		// Artifacts are never published; previously committed cache entries
		// stay self-consistent and are reused on the next attempt.
		sortErrors(moduleErrs)
		return nil, &BuildErrors{Errs: moduleErrs}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// 7. Synthesize artifacts deterministically.
	artifacts := b.synthesize(g, entries, outputs)

	// 8. Publish atomically: no half-finished release is ever visible.
	store := NewArtifactStore(cfg.OutputDir)
	relDir, err := store.Publish(ctx, artifacts)
	if err != nil {
		return nil, fmt.Errorf("publish artifacts: %w", err)
	}

	sort.Slice(diags, func(i, j int) bool {
		if diags[i].Kind != diags[j].Kind {
			return diags[i].Kind < diags[j].Kind
		}
		return diags[i].Module < diags[j].Module
	})

	return &Report{
		Modules:     len(g.Order),
		Transformed: misses,
		CacheHits:   hits,
		Cycles:      g.Cycles,
		Diagnostics: diags,
		Artifacts:   artifacts,
		ReleaseDir:  relDir,
		CacheReused: hits > 0,
	}, nil
}

// transformOne returns transformed content for one module, using the cache or
// the pipeline. Committed entries are persisted under the cross-process lock
// with an immediate index reload, so concurrent processes never overwrite
// each other and an interrupted build loses at most uncommitted work.
func (b *Builder) transformOne(ctx context.Context, cache *Cache, g *Graph, id, key, pipelineID string, sccHash map[int]string) ([]byte, bool, error) {
	if e, ok := cache.Lookup(key); ok {
		content, err := cache.ReadBlob(e.Blob)
		if err == nil {
			return content, true, nil
		}
		// Missing blob: fall through to recomputation (safe degradation).
	}
	type result struct {
		content []byte
		err     error
	}
	res := globalFlight.do(cache.dir+"\x00"+key, func() any {
		// Re-check cache inside the flight: another goroutine may have just
		// committed, or a concurrent process left the answer for us.
		if e, ok := cache.Lookup(key); ok {
			if content, err := cache.ReadBlob(e.Blob); err == nil {
				return result{content: content}
			}
		}
		m := g.Modules[id]
		out, err := b.cfg.Pipeline.Run(ctx, id, m.Content)
		if err != nil {
			return result{err: err}
		}
		blob, err := cache.PutBlob(out)
		if err != nil {
			return result{err: &ModuleError{Module: id, Stage: "cache-write", Err: err}}
		}
		entry := &CacheEntry{
			Key:         key,
			ModuleID:    id,
			ContentHash: hashContent(m.Content),
			DepsKey:     sccKey(g, id, sccHash),
			PipelineID:  pipelineID,
			Blob:        blob,
			CreatedAt:   time.Now().UTC(),
		}
		// lock -> reload -> merge -> rename -> unlock keeps the on-disk
		// index consistent for every observer.
		lock, lerr := cache.Lock(ctx)
		if lerr != nil {
			return result{err: &ModuleError{Module: id, Stage: "cache-lock", Err: lerr}}
		}
		commitErr := func() error {
			if err := lock.Reload(); err != nil {
				return &ModuleError{Module: id, Stage: "cache-reload", Err: err}
			}
			if err := cache.CommitLocked(ctx, entry); err != nil {
				return &ModuleError{Module: id, Stage: "cache-commit", Err: err}
			}
			return nil
		}()
		if uerr := lock.Unlock(); commitErr == nil && uerr != nil {
			commitErr = &ModuleError{Module: id, Stage: "cache-unlock", Err: uerr}
		}
		if commitErr != nil {
			return result{err: commitErr}
		}
		return result{content: out}
	}).(result)
	if res.err != nil {
		return nil, false, res.err
	}
	return res.content, false, nil
}

func (b *Builder) synthesize(g *Graph, entries []string, outputs map[string][]byte) []Artifact {
	switch b.cfg.Mode {
	case OutputSplit:
		artifacts := make([]Artifact, 0, len(entries))
		for _, e := range entries {
			mods := closureOrder(g, map[string]bool{e: true})
			name := "entries/" + encodeName(e) + ".out"
			content := assembleArtifact(name, []string{e}, mods, outputs)
			artifacts = append(artifacts, Artifact{Name: name, Entries: []string{e}, Content: content})
		}
		return artifacts
	default:
		mods := closureOrder(g, stringSet(entries))
		content := assembleArtifact("bundle/bundle.out", entries, mods, outputs)
		return []Artifact{{Name: "bundle/bundle.out", Entries: entries, Content: content}}
	}
}

// closureOrder returns modules reachable from roots through the ORIGINAL
// declared edges (cycles fully included), ordered by the deterministic
// topological build order.
func closureOrder(g *Graph, roots map[string]bool) []string {
	seen := map[string]bool{}
	var stack []string
	for r := range roots {
		stack = append(stack, r)
	}
	for len(stack) > 0 {
		v := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[v] {
			continue
		}
		seen[v] = true
		for _, dep := range g.Modules[v].Deps {
			if !seen[dep] {
				stack = append(stack, dep)
			}
		}
	}
	var out []string
	for _, id := range g.Order {
		if seen[id] {
			out = append(out, id)
		}
	}
	return out
}

func assembleArtifact(name string, entries, mods []string, outputs map[string][]byte) []byte {
	es := uniqueSorted(entries)
	var buf bytes.Buffer
	buf.WriteString("// artifact generated by incremental-bundler-go; do not edit\n")
	buf.WriteString("// artifact: ")
	buf.WriteString(name)
	buf.WriteByte('\n')
	buf.WriteString("// entries: ")
	buf.WriteString(strings.Join(es, ","))
	buf.WriteByte('\n')
	buf.WriteString("// modules: ")
	buf.WriteString(fmt.Sprint(len(mods)))
	buf.WriteByte('\n')
	for _, id := range mods {
		buf.WriteString("// ===== begin module: ")
		buf.WriteString(id)
		buf.WriteString(" =====\n")
		c := outputs[id]
		buf.Write(c)
		if len(c) == 0 || c[len(c)-1] != '\n' {
			buf.WriteByte('\n')
		}
		buf.WriteString("// ===== end module: ")
		buf.WriteString(id)
		buf.WriteString(" =====\n")
	}
	return buf.Bytes()
}

// encodeName maps an arbitrary module id to a collision-free deterministic
// file name component: [A-Za-z0-9._-] are kept, every other byte becomes
// "_xx" lowercase hex (so '/' -> "_2f" and no two ids can collide).
func encodeName(id string) string {
	var b strings.Builder
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '.', c == '_', c == '-':
			b.WriteByte(c)
		default:
			b.WriteByte('_')
			const hex = "0123456789abcdef"
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&0xf])
		}
	}
	return b.String()
}

func uniqueSorted(xs []string) []string {
	set := map[string]bool{}
	for _, x := range xs {
		set[x] = true
	}
	out := make([]string, 0, len(set))
	for x := range set {
		out = append(out, x)
	}
	sort.Strings(out)
	return out
}

func stringSet(xs []string) map[string]bool {
	m := map[string]bool{}
	for _, x := range xs {
		m[x] = true
	}
	return m
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func sortErrors(errs []error) {
	sort.Slice(errs, func(i, j int) bool { return errs[i].Error() < errs[j].Error() })
}

// ---- process-wide singleflight for cache keys ----

type flightCall struct {
	done chan struct{}
	val  any
}

type flightGroup struct {
	mu sync.Mutex
	m  map[string]*flightCall
}

var globalFlight = flightGroup{m: map[string]*flightCall{}}

func (g *flightGroup) do(key string, fn func() any) any {
	g.mu.Lock()
	if c, ok := g.m[key]; ok {
		g.mu.Unlock()
		<-c.done
		return c.val
	}
	c := &flightCall{done: make(chan struct{})}
	g.m[key] = c
	g.mu.Unlock()
	c.val = fn()
	close(c.done)
	g.mu.Lock()
	delete(g.m, key)
	g.mu.Unlock()
	return c.val
}
