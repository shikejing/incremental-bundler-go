package bundler

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestCorruptVersionFallsBackToFullRebuild(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{Entries: []string{"main.js"}, Transforms: []Transform{upper()}, CacheDir: dir}
	r1, err := New(cfg).Build(context.Background(), baseSources())
	if err != nil {
		t.Fatalf("build1: %v", err)
	}
	// 写入不兼容版本号，模拟格式升级/损坏。
	if err := os.WriteFile(filepath.Join(dir, "VERSION"), []byte("999\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r2, err := New(cfg).Build(context.Background(), baseSources())
	if err != nil {
		t.Fatalf("build with incompatible cache must not fail: %v", err)
	}
	if len(r2.CacheMisses) != 3 {
		t.Fatalf("expected full rebuild after version mismatch, misses=%v", r2.CacheMisses)
	}
	if !bytes.Equal(r1.Artifacts[0].Content, r2.Artifacts[0].Content) {
		t.Fatal("output changed after cache reset")
	}
	// 再次构建应恢复命中。
	r3, err := New(cfg).Build(context.Background(), baseSources())
	if err != nil {
		t.Fatalf("build3: %v", err)
	}
	if len(r3.CacheHits) != 3 {
		t.Fatalf("cache should be usable after reset, hits=%v", r3.CacheHits)
	}
}

func TestCorruptEntryTreatedAsMiss(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{Entries: []string{"main.js"}, Transforms: []Transform{upper()}, CacheDir: dir}
	r1, err := New(cfg).Build(context.Background(), baseSources())
	if err != nil {
		t.Fatalf("build1: %v", err)
	}
	// 随机破坏一个缓存项内容。
	entries, err := os.ReadDir(filepath.Join(dir, "entries"))
	if err != nil || len(entries) == 0 {
		t.Fatalf("no cache entries: %v", err)
	}
	victim := filepath.Join(dir, "entries", entries[0].Name())
	if err := os.WriteFile(victim, []byte("garbage-without-valid-checksum"), 0o644); err != nil {
		t.Fatal(err)
	}
	r2, err := New(cfg).Build(context.Background(), baseSources())
	if err != nil {
		t.Fatalf("corrupt entry must not crash build: %v", err)
	}
	if len(r2.CacheMisses) != 1 || len(r2.CacheHits) != 2 {
		t.Fatalf("expected exactly 1 miss after corruption: hits=%v misses=%v", r2.CacheHits, r2.CacheMisses)
	}
	if !bytes.Equal(r1.Artifacts[0].Content, r2.Artifacts[0].Content) {
		t.Fatal("output changed after recovering from corrupt entry")
	}
}

func TestConcurrentBuildsSharedCache(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{Entries: []string{"main.js"}, Transforms: []Transform{upper()}, CacheDir: dir, Workers: 4}
	// 预热缓存。
	if _, err := New(cfg).Build(context.Background(), baseSources()); err != nil {
		t.Fatalf("warmup: %v", err)
	}
	const n = 16
	results := make([]*BuildResult, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r, err := New(cfg).Build(context.Background(), baseSources())
			results[i] = r
			errs[i] = err
		}(i)
	}
	wg.Wait()
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("concurrent build %d failed: %v", i, errs[i])
		}
		if !bytes.Equal(results[0].Artifacts[0].Content, results[i].Artifacts[0].Content) {
			t.Fatalf("concurrent build %d produced different output", i)
		}
	}
	// 并发增量与全量混合：一半构建器修改 util，一半不变，各自结果一致。
	errs2 := make([]error, n)
	outs := make([][]byte, n)
	changed := baseSources()
	changed[2] = src("util.js", `export const util = 99;`)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sources := baseSources()
			if i%2 == 0 {
				sources = changed
			}
			r, err := New(cfg).Build(context.Background(), sources)
			errs2[i] = err
			if err == nil {
				outs[i] = r.Artifacts[0].Content
			}
		}(i)
	}
	wg.Wait()
	for i := 0; i < n; i++ {
		if errs2[i] != nil {
			t.Fatalf("mixed build %d failed: %v", i, errs2[i])
		}
		if i > 1 && i%2 == 0 && !bytes.Equal(outs[0], outs[i]) {
			t.Fatalf("changed-input build %d diverged", i)
		}
		if i%2 == 1 && !bytes.Equal(outs[1], outs[i]) {
			t.Fatalf("unchanged-input build %d diverged", i)
		}
	}
}

func TestCancellationResumesFromConsistentState(t *testing.T) {
	dir := t.TempDir()
	// 慢转换：lib.js 转换期间取消。
	started := make(chan struct{})
	var once sync.Once
	slow := TransformFunc{StepName: "slow", Fn: func(ctx context.Context, id string, c []byte) ([]byte, error) {
		if id == "lib.js" {
			isFirst := false
			once.Do(func() { close(started); isFirst = true })
			if isFirst {
				// 仅首次调用阻塞等待取消；后续（重构建）立即通过。
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(5 * time.Second):
				}
			}
		}
		return c, nil
	}}
	cfg := Config{Entries: []string{"main.js"}, Transforms: []Transform{slow, upper()}, CacheDir: dir, Workers: 8}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-started
		cancel()
	}()
	_, err := New(cfg).Build(ctx, baseSources())
	if err == nil {
		t.Fatal("expected cancellation error")
	}
	// 重新构建：已提交的缓存（util.js）应被复用，最终产物与全量构建一致。
	r, err := New(cfg).Build(context.Background(), baseSources())
	if err != nil {
		t.Fatalf("rebuild after cancel: %v", err)
	}
	full, err := New(Config{Entries: cfg.Entries, Transforms: cfg.Transforms, CacheDir: t.TempDir()}).Build(context.Background(), baseSources())
	if err != nil {
		t.Fatalf("full build: %v", err)
	}
	if !bytes.Equal(r.Artifacts[0].Content, full.Artifacts[0].Content) {
		t.Fatal("resumed build output differs from clean full build")
	}
}

func TestArtifactDirAtomicAndStaleCleanup(t *testing.T) {
	cacheDir := t.TempDir()
	outDir := t.TempDir()
	cfg := Config{Entries: []string{"e1.js", "e2.js"}, Transforms: []Transform{upper()},
		Split: SplitPerEntry, CacheDir: cacheDir, OutDir: outDir}
	sources := []Source{
		src("e1.js", `import "./shared.js";`),
		src("e2.js", `import "./shared.js";`),
		src("shared.js", `export const s = 1;`),
	}
	if _, err := New(cfg).Build(context.Background(), sources); err != nil {
		t.Fatalf("build1: %v", err)
	}
	for _, name := range []string{"e1.bundle.js", "e2.bundle.js"} {
		if _, err := os.Stat(filepath.Join(outDir, name)); err != nil {
			t.Fatalf("artifact %s missing: %v", name, err)
		}
	}
	// 删除入口 e2：其旧产物必须被清理，不得残留。
	cfg.Entries = []string{"e1.js"}
	if _, err := New(cfg).Build(context.Background(), sources); err != nil {
		t.Fatalf("build2: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outDir, "e2.bundle.js")); !os.IsNotExist(err) {
		t.Fatal("stale artifact e2.bundle.js not cleaned up")
	}
	if _, err := os.Stat(filepath.Join(outDir, "e1.bundle.js")); err != nil {
		t.Fatal("e1.bundle.js should still exist")
	}
	// 输出目录中不得残留临时文件。
	entries, _ := os.ReadDir(outDir)
	for _, e := range entries {
		if len(e.Name()) > 0 && e.Name()[0] == '.' && e.Name() != ".manifest" {
			t.Fatalf("temp file leaked: %s", e.Name())
		}
	}
}

func TestMultiProcessStyleCacheSharing(t *testing.T) {
	// 模拟多进程：多个独立 Cache 实例（无共享内存）并发读写同一目录。
	dir := t.TempDir()
	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c, err := OpenCache(dir)
			if err != nil {
				errs[i] = err
				return
			}
			key := fmt.Sprintf("key-%d", i%3) // 有意制造同键并发写
			payload := []byte(fmt.Sprintf("payload-%d", i%3))
			if err := c.Put(key, payload); err != nil {
				errs[i] = err
				return
			}
			got, ok, err := c.Get(key)
			if err != nil {
				errs[i] = err
				return
			}
			if ok && !bytes.Equal(got, payload) {
				errs[i] = fmt.Errorf("read inconsistent payload for %s", key)
			}
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("worker %d: %v", i, err)
		}
	}
}
