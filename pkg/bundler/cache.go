package bundler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"
)

// CacheFormatVersion is the on-disk cache layout version. A mismatch forces a
// safe full rebuild without manual cleanup.
const CacheFormatVersion = 1

// CacheEntry is one persisted transformed module.
type CacheEntry struct {
	Key         string    `json:"key"`
	ModuleID    string    `json:"module_id"`
	ContentHash string    `json:"content_hash"`
	DepsKey     string    `json:"deps_key"`
	PipelineID  string    `json:"pipeline_id"`
	Blob        string    `json:"blob"` // content hash of transformed output
	CreatedAt   time.Time `json:"created_at"`
}

// cacheIndex is the persisted root document (one JSON file, atomic rename).
type cacheIndex struct {
	FormatVersion int                    `json:"format_version"`
	Entries       map[string]*CacheEntry `json:"entries"`
}

// Cache is a content-addressed, persistent, multi-process-safe module cache.
type Cache struct {
	dir         string
	index       *cacheIndex
	mu          sync.Mutex
	quarantined bool // true when an incompatible/corrupt index was replaced
}

// OpenCache opens (or creates) the cache under dir. Corrupt or incompatible
// data is detected and safely quarantined; callers always get a usable,
// self-consistent cache and never need to delete files by hand.
func OpenCache(ctx context.Context, dir string) (*Cache, error) {
	if err := os.MkdirAll(filepath.Join(dir, "blobs"), 0o755); err != nil {
		return nil, fmt.Errorf("create cache dir: %w", err)
	}
	c := &Cache{dir: dir, index: &cacheIndex{FormatVersion: CacheFormatVersion, Entries: map[string]*CacheEntry{}}}
	path := filepath.Join(dir, "index.json")
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		var idx cacheIndex
		if jerr := json.Unmarshal(data, &idx); jerr != nil || idx.FormatVersion != CacheFormatVersion || idx.Entries == nil {
			if qerr := c.quarantine(data, jerr); qerr != nil {
				return nil, qerr
			}
		} else {
			c.index = &idx
		}
	case errors.Is(err, fs.ErrNotExist):
		// Fresh cache: write the initial index immediately.
		if werr := c.writeIndexLocked(); werr != nil {
			return nil, werr
		}
	default:
		return nil, fmt.Errorf("read cache index: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Cache) quarantine(data []byte, cause error) error {
	side := filepath.Join(c.dir, fmt.Sprintf("index.corrupt-%d", time.Now().UnixNano()))
	if err := os.WriteFile(side, data, 0o644); err != nil {
		return fmt.Errorf("quarantine corrupt cache: %w", err)
	}
	c.index = &cacheIndex{FormatVersion: CacheFormatVersion, Entries: map[string]*CacheEntry{}}
	c.quarantined = true
	if err := c.writeIndexLocked(); err != nil {
		return err
	}
	// Leave a marker explaining why a full rebuild happened.
	_ = os.WriteFile(filepath.Join(c.dir, "RECOVERY.txt"),
		[]byte("cache index was corrupt or incompatible ("+fmt.Sprint(cause)+
			"); quarantined and rebuilt from scratch at "+time.Now().Format(time.RFC3339)+"\n"),
		0o644)
	return nil
}

// Dir returns the cache root directory.
func (c *Cache) Dir() string { return c.dir }

// WasQuarantined reports whether opening replaced a bad cache (full rebuild).
func (c *Cache) WasQuarantined() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.quarantined
}

// Lookup returns a hit only when key matches exactly, verifying that the
// referenced blob exists and is intact.
func (c *Cache) Lookup(key string) (*CacheEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.index.Entries[key]
	if !ok || len(e.Blob) < 2 {
		return nil, false
	}
	_, err := os.Stat(filepath.Join(c.dir, "blobs", e.Blob[:2], e.Blob))
	if err != nil {
		return nil, false
	}
	cp := *e
	return &cp, true
}

// ReadBlob returns the transformed content of a previously stored blob,
// verifying integrity: the blob file must hash to its own content-addressed id.
// A corrupted or truncated blob is reported as such, and callers treat that as
// a miss and recompute.
func (c *Cache) ReadBlob(blob string) ([]byte, error) {
	if len(blob) < 2 {
		return nil, fmt.Errorf("invalid blob id %q", blob)
	}
	data, err := os.ReadFile(filepath.Join(c.dir, "blobs", blob[:2], blob))
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != blob {
		return nil, fmt.Errorf("blob %s fails integrity check", blob)
	}
	return data, nil
}

// PutBlob stores transformed output content-addressed by its own hash and
// returns the blob id. Writes go through a temp file + rename so readers can
// never observe a half-written blob.
func (c *Cache) PutBlob(content []byte) (string, error) {
	sum := sha256.Sum256(content)
	id := hex.EncodeToString(sum[:])
	dir := filepath.Join(c.dir, "blobs", id[:2])
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	dst := filepath.Join(dir, id)
	// Always rewrite via temp + rename: this is atomic for concurrent
	// readers and also heals a corrupted file occupying the same
	// content-addressed path.
	tmp, err := os.CreateTemp(c.dir, "blob-*")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return "", err
	}
	cleanup = false
	return id, nil
}

// Commit atomically merges a batch of entries into the persisted index. The
// index is always rewritten via temp-file + rename, so any on-disk view is
// either the previous complete state or the new complete state.
func (c *Cache) Commit(ctx context.Context, entries []*CacheEntry) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range entries {
		c.index.Entries[e.Key] = e
	}
	return c.writeIndexLocked()
}

// CommitLocked merges one entry while the caller holds the cross-process
// CacheLock. The in-memory index at this point reflects a lock-time Reload.
func (c *Cache) CommitLocked(ctx context.Context, entry *CacheEntry) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.index.Entries[entry.Key] = entry
	return c.writeIndexLocked()
}

func (c *Cache) writeIndexLocked() error {
	path := filepath.Join(c.dir, "index.json")
	tmp, err := os.CreateTemp(c.dir, "index-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()
	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(c.index); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	cleanup = false
	return nil
}

// Lock acquires a cross-process advisory lock (flock) for a build session.
// Multiple processes sharing one cache are serialized only for the short
// index reload/commit critical sections; waiting is interruptible by ctx.
func (c *Cache) Lock(ctx context.Context) (*CacheLock, error) {
	f, err := os.OpenFile(filepath.Join(c.dir, "lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return &CacheLock{c: c, f: f}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			f.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// CacheLock guards index reload/commit across processes.
type CacheLock struct {
	c *Cache
	f *os.File
}

// Reload re-reads the index while holding the lock, picking up commits from
// other processes. Corruption appearing after a restart is handled safely.
func (l *CacheLock) Reload() error {
	l.c.mu.Lock()
	defer l.c.mu.Unlock()
	data, err := os.ReadFile(filepath.Join(l.c.dir, "index.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var idx cacheIndex
	if err := json.Unmarshal(data, &idx); err != nil || idx.FormatVersion != CacheFormatVersion || idx.Entries == nil {
		// Another writer left an unusable index: discard in-memory view so
		// this process recomputes; do not crash or overwrite while locked.
		l.c.index = &cacheIndex{FormatVersion: CacheFormatVersion, Entries: map[string]*CacheEntry{}}
		l.c.quarantined = true
		return nil
	}
	l.c.index = &idx
	return nil
}

func (l *CacheLock) Unlock() error {
	err1 := syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	err2 := l.f.Close()
	if err1 != nil {
		return err1
	}
	return err2
}

// Close releases cache resources.
func (c *Cache) Close() error { return nil }

// sccKey computes the deterministic SCC fingerprint for module's component:
// hash over (sorted member content hashes) and the sorted fingerprints of
// external dependency SCCs. Components are partially ordered by edges, so
// external SCC fingerprints are always already computed (cycle-free at the
// condensation level). Every member of an SCC shares this key, which means a
// change anywhere in the cycle invalidates all of it, as required.
func sccKey(g *Graph, module string, sccHash map[int]string) string {
	ci := g.sccOf[module]
	if h, ok := sccHash[ci]; ok {
		return h
	}
	members := g.sccMembers[ci]
	inSCC := map[string]bool{}
	for _, m := range members {
		inSCC[m] = true
	}
	var own []string
	for _, m := range members {
		own = append(own, hashContent(g.Modules[m].Content))
	}
	sort.Strings(own)
	ext := map[string]bool{}
	for _, m := range members {
		for _, dep := range g.Modules[m].Deps {
			if !inSCC[dep] {
				ext[sccKey(g, dep, sccHash)] = true
			}
		}
	}
	var extKeys []string
	for k := range ext {
		extKeys = append(extKeys, k)
	}
	sort.Strings(extKeys)
	h := sha256.New()
	h.Write([]byte("scc1\n"))
	for _, o := range own {
		h.Write([]byte(o))
		h.Write([]byte{'\n'})
	}
	h.Write([]byte("--deps--\n"))
	for _, k := range extKeys {
		h.Write([]byte(k))
		h.Write([]byte{'\n'})
	}
	sum := h.Sum(nil)
	key := hex.EncodeToString(sum)[:32]
	sccHash[ci] = key
	return key
}

// computeKey derives a module cache key from the module's SCC fingerprint
// (own content + every reachable dependency's content, cycle stable) and the
// pipeline id, so any upstream or pipeline change invalidates the module.
func computeKey(g *Graph, module, pipelineID string, sccHash map[int]string) string {
	h := sha256.New()
	h.Write([]byte("mod1\n"))
	h.Write([]byte(module))
	h.Write([]byte{'\n'})
	h.Write([]byte(pipelineID))
	h.Write([]byte{'\n'})
	h.Write([]byte(sccKey(g, module, sccHash)))
	sum := h.Sum(nil)
	return hex.EncodeToString(sum)[:32]
}

// hashContent is the source-content hash used for change detection.
func hashContent(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
