package bundler

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func slowBuilder(src Source, cacheDir, outDir string, delay time.Duration) *Builder {
	return New(Config{
		Source:    src,
		CacheDir:  cacheDir,
		OutputDir: outDir,
		Pipeline:  NewPipeline(slowTransform{delay: delay}, TrimTransform{}),
		Mode:      OutputSingle,
		Workers:   8,
	})
}

type slowTransform struct{ delay time.Duration }

func (slowTransform) Name() string { return "slow" }
func (s slowTransform) Apply(ctx context.Context, in []byte) ([]byte, error) {
	select {
	case <-time.After(s.delay):
		return in, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestConcurrentBuildsShareCache(t *testing.T) {
	dir := t.TempDir()
	cacheDir := filepath.Join(dir, "cache")
	const n = 5
	var wg sync.WaitGroup
	errs := make([]error, n)
	reps := make([]*Report, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			src := NewMemorySource().
				Add("a", fmt.Sprintf("alpha-%d\n// @import b", i%2)).
				Add("b", "beta")
			reps[i], errs[i] = slowBuilder(src, cacheDir, filepath.Join(dir, fmt.Sprintf("o%d", i)), 20*time.Millisecond).
				Build(context.Background())
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent build %d: %v", i, err)
		}
	}
	// All outputs must be internally consistent artifacts.
	for i, r := range reps {
		if len(r.Artifacts) != 1 {
			t.Fatalf("build %d artifacts=%d", i, len(r.Artifacts))
		}
	}
	// Immediate sequential rebuild with a fixed source hits the cache.
	src := NewMemorySource().Add("a", "alpha-0\n// @import b").Add("b", "beta")
	r, err := slowBuilder(src, cacheDir, filepath.Join(dir, "ofinal"), time.Millisecond).Build(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if r.CacheHits != 2 || r.Transformed != 0 {
		t.Fatalf("post-concurrency rebuild want 2 hits, got hits=%d xform=%d", r.CacheHits, r.Transformed)
	}
}

func TestCancelledBuildResumesFromConsistentState(t *testing.T) {
	dir := t.TempDir()
	cacheDir, outDir := filepath.Join(dir, "cache"), filepath.Join(dir, "out")
	src := NewMemorySource().
		Add("a", "alpha\n// @import b").
		Add("b", "beta\n// @import c").
		Add("c", "gamma")

	// Race a cancellation against a slow build; retry until we observe a
	// partially completed (but self-consistent) cache.
	cancelledAtLeastOnce := false
	for attempt := 0; attempt < 20; attempt++ {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			time.Sleep(time.Duration(5+attempt*2) * time.Millisecond)
			cancel()
			close(done)
		}()
		_ = done
		b := New(Config{
			Source: src, CacheDir: cacheDir, OutputDir: outDir,
			Pipeline: NewPipeline(slowTransform{delay: 15 * time.Millisecond}),
			Mode:     OutputSingle, Entries: []string{"a"},
		})
		_, err := b.Build(ctx)
		cancel()
		if err != nil {
			cancelledAtLeastOnce = true
			break
		}
		// Build may have completed first; the following resume test is then
		// performed with a changed file below.
		break
	}
	if !cancelledAtLeastOnce {
		// Deterministic fallback: cancel before starting.
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		b := New(Config{
			Source: src, CacheDir: cacheDir, OutputDir: outDir,
			Pipeline: NewPipeline(slowTransform{delay: 5 * time.Millisecond}),
			Mode:     OutputSingle, Entries: []string{"a"},
		})
		if _, err := b.Build(ctx); err == nil {
			t.Fatal("pre-cancelled build should fail")
		}
	}

	// No half-written current/ may be visible.
	if target, err := os.Readlink(filepath.Join(outDir, "current")); err == nil {
		if _, err := os.Stat(filepath.Join(outDir, target, "MANIFEST.json")); err != nil {
			t.Fatalf("current points at incomplete release %s: %v", target, err)
		}
	}

	// Resume: fresh builder on the same cache. Committed modules must be
	// reused without reapplying transforms; final artifact must be complete.
	r := buildOnce(t, src, cacheDir, filepath.Join(dir, "out-resumed"), OutputSingle, "a")
	if r.Transformed+r.CacheHits != 3 {
		t.Fatalf("resume must cover exactly 3 modules, got hits=%d xform=%d", r.CacheHits, r.Transformed)
	}
	content := string(r.Artifacts[0].Content)
	for _, want := range []string{"alpha", "beta", "gamma"} {
		if !contains(content, want) {
			t.Fatalf("resumed artifact missing %q", want)
		}
	}

	// A second resume build must be all hits: no transform applied twice.
	r2 := buildOnce(t, src, cacheDir, filepath.Join(dir, "out-resumed2"), OutputSingle, "a")
	if r2.CacheHits != 3 || r2.Transformed != 0 {
		t.Fatalf("final state should be fully cached, got hits=%d xform=%d", r2.CacheHits, r2.Transformed)
	}
	if string(r2.Artifacts[0].Content) != content {
		t.Fatal("resumed outputs are not deterministic")
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestDirSourceAndRenames(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(rel, body string) {
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(rel)), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("pkg/a.src", "A-body\n// @import pkg/b")
	write("pkg/b.src", "B-body")
	write("ignore.txt", "not a module")

	src := NewDirSource(root)
	files, err := src.ReadAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("want 2 .src files, got %d: %+v", len(files), files)
	}

	dir := t.TempDir()
	r := buildOnce(t, src, filepath.Join(dir, "c"), filepath.Join(dir, "o"), OutputSingle)
	if !contains(string(r.Artifacts[0].Content), "A-body") ||
		!contains(string(r.Artifacts[0].Content), "B-body") {
		t.Fatal("dir build missing content")
	}

	// Rename b -> c and update import.
	if err := os.Rename(filepath.Join(root, "pkg/b.src"), filepath.Join(root, "pkg/c.src")); err != nil {
		t.Fatal(err)
	}
	write("pkg/a.src", "A-body\n// @import pkg/c")
	r = buildOnce(t, src, filepath.Join(dir, "c"), filepath.Join(dir, "o2"), OutputSingle)
	out := string(r.Artifacts[0].Content)
	if contains(out, "begin module: pkg/b ") {
		t.Fatal("renamed-away module still bundled")
	}
	if !contains(out, "begin module: pkg/c ") {
		t.Fatal("renamed-in module missing")
	}
}
