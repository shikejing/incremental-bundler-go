package bundler

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func warmCacheBuild(t *testing.T, dir string) {
	t.Helper()
	src := NewMemorySource().
		Add("a", "alpha\n// @import b").
		Add("b", "beta")
	buildOnce(t, src, filepath.Join(dir, "cache"), filepath.Join(dir, "out"), OutputSingle, "a")
}

func TestCacheCorruptionRecoversToFullRebuild(t *testing.T) {
	dir := t.TempDir()
	warmCacheBuild(t, dir)
	cacheDir := filepath.Join(dir, "cache")

	// Garbage index bytes: must not crash; next build is a full rebuild and
	// leaves a usable cache again.
	if err := os.WriteFile(filepath.Join(cacheDir, "index.json"),
		[]byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := NewMemorySource().
		Add("a", "alpha\n// @import b").
		Add("b", "beta")
	r := buildOnce(t, src, cacheDir, filepath.Join(dir, "out2"), OutputSingle, "a")
	if r.Transformed != 2 || r.CacheHits != 0 {
		t.Fatalf("corrupt cache must force full rebuild, got hits=%d xform=%d", r.CacheHits, r.Transformed)
	}
	// Corrupt file quarantined, recovery note present.
	matches, _ := filepath.Glob(filepath.Join(cacheDir, "index.corrupt-*"))
	if len(matches) != 1 {
		t.Fatalf("want 1 quarantined index, got %v", matches)
	}
	if _, err := os.Stat(filepath.Join(cacheDir, "RECOVERY.txt")); err != nil {
		t.Fatalf("RECOVERY.txt missing: %v", err)
	}

	// Rebuild again with recovered cache: hits again.
	r = buildOnce(t, src, cacheDir, filepath.Join(dir, "out3"), OutputSingle, "a")
	if r.CacheHits != 2 {
		t.Fatalf("recovered cache should hit, got %d", r.CacheHits)
	}
}

func TestCacheTruncatedJSONRecovers(t *testing.T) {
	dir := t.TempDir()
	warmCacheBuild(t, dir)
	cacheDir := filepath.Join(dir, "cache")
	data, err := os.ReadFile(filepath.Join(cacheDir, "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, "index.json"), data[:len(data)/2], 0o644); err != nil {
		t.Fatal(err)
	}
	src := NewMemorySource().
		Add("a", "alpha\n// @import b").
		Add("b", "beta")
	r := buildOnce(t, src, cacheDir, filepath.Join(dir, "out4"), OutputSingle, "a")
	if r.Transformed != 2 {
		t.Fatalf("truncated index must force rebuild, xform=%d", r.Transformed)
	}
}

func TestCacheVersionMismatchRecovers(t *testing.T) {
	dir := t.TempDir()
	warmCacheBuild(t, dir)
	cacheDir := filepath.Join(dir, "cache")
	if err := os.WriteFile(filepath.Join(cacheDir, "index.json"),
		[]byte(`{"format_version":999,"entries":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	src := NewMemorySource().
		Add("a", "alpha\n// @import b").
		Add("b", "beta")
	r := buildOnce(t, src, cacheDir, filepath.Join(dir, "out5"), OutputSingle, "a")
	if r.Transformed != 2 {
		t.Fatalf("version mismatch must force rebuild, xform=%d", r.Transformed)
	}
}

func TestCacheLockExcludesOtherProcess(t *testing.T) {
	dir := t.TempDir()
	c, err := OpenCache(context.Background(), filepath.Join(dir, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	l1, err := c.Lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	got := make(chan error, 1)
	go func() {
		l2, err := c.Lock(ctx)
		if err != nil {
			got <- err
			return
		}
		got <- l2.Unlock()
	}()

	// Waiting lock respects cancellation instead of blocking forever.
	cancel()
	if err := <-got; err != context.Canceled {
		t.Fatalf("want context.Canceled, got %v", err)
	}

	// After release, another lock can be acquired.
	if err := l1.Unlock(); err != nil {
		t.Fatal(err)
	}
	l3, err := c.Lock(context.Background())
	if err != nil {
		t.Fatalf("lock after unlock failed: %v", err)
	}
	if err := l3.Unlock(); err != nil {
		t.Fatal(err)
	}
}

func TestCorruptBlobTriggersRecompute(t *testing.T) {
	dir := t.TempDir()
	warmCacheBuild(t, dir)
	cacheDir := filepath.Join(dir, "cache")

	// Corrupt one blob on disk: index still references it, but content hash
	// no longer matches. Build must transparently recompute instead of
	// serving torn bytes or failing.
	var target string
	err := filepath.Walk(filepath.Join(cacheDir, "blobs"), func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		target = path
		return nil
	})
	if err != nil || target == "" {
		t.Fatalf("blob not found: %v", err)
	}
	if err := os.WriteFile(target, []byte("CORRUPTED-BYTES"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := NewMemorySource().
		Add("a", "alpha\n// @import b").
		Add("b", "beta")
	r := buildOnce(t, src, cacheDir, filepath.Join(dir, "out-blob"), OutputSingle, "a")
	if r.CacheHits < 1 || r.Transformed < 1 {
		t.Fatalf("corrupt blob should force partial recompute, got hits=%d xform=%d", r.CacheHits, r.Transformed)
	}
	if !contains(string(r.Artifacts[0].Content), "beta") {
		t.Fatal("artifact missing recomputed content")
	}
	// Recomputed blob heals: next build fully cached.
	r2 := buildOnce(t, src, cacheDir, filepath.Join(dir, "out-blob2"), OutputSingle, "a")
	if r2.CacheHits != 2 {
		t.Fatalf("healed cache should fully hit, got %d", r2.CacheHits)
	}
}
