package bundler

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testPipeline() *Pipeline {
	return NewPipeline(TrimTransform{}, BannerTransform{Text: "bundled"})
}

func buildOnce(t *testing.T, src Source, cacheDir, outDir string, mode OutputMode, entries ...string) *Report {
	t.Helper()
	b := New(Config{
		Source:    src,
		CacheDir:  cacheDir,
		OutputDir: outDir,
		Pipeline:  testPipeline(),
		Mode:      mode,
		Entries:   entries,
		Workers:   1,
	})
	rep, err := b.Build(context.Background())
	if err != nil {
		t.Fatalf("build failed: %v", err)
	}
	return rep
}

func chainSource() *MemorySource {
	return NewMemorySource().
		Add("a", "alpha\n// @import b").
		Add("b", "beta\n// @import c").
		Add("c", "gamma")
}

func TestFullThenIncrementalByteIdentical(t *testing.T) {
	dir := t.TempDir()
	src := chainSource()

	r1 := buildOnce(t, src, filepath.Join(dir, "cache"), filepath.Join(dir, "out"), OutputSingle, "a")
	if r1.CacheHits != 0 || r1.Transformed != 3 {
		t.Fatalf("first build want 0 hits/3 transforms, got hits=%d xform=%d", r1.CacheHits, r1.Transformed)
	}
	first := r1.Artifacts[0].Content

	// New Builder instance, same cache/output: process-restart-like reuse.
	r2 := buildOnce(t, src, filepath.Join(dir, "cache"), filepath.Join(dir, "out"), OutputSingle, "a")
	if r2.CacheHits != 3 || r2.Transformed != 0 {
		t.Fatalf("second build want 3 hits/0 transforms, got hits=%d xform=%d", r2.CacheHits, r2.Transformed)
	}
	if string(r2.Artifacts[0].Content) != string(first) {
		t.Fatalf("incremental output differs from full build")
	}
	// The release id must be identical (same current symlink target).
	if r1.ReleaseDir != r2.ReleaseDir {
		t.Fatalf("release dirs differ: %s vs %s", r1.ReleaseDir, r2.ReleaseDir)
	}
}

func TestUpstreamChangeInvalidatesDownstream(t *testing.T) {
	dir := t.TempDir()
	src := chainSource()
	cacheDir, outDir := filepath.Join(dir, "cache"), filepath.Join(dir, "out")

	buildOnce(t, src, cacheDir, outDir, OutputSingle, "a")

	// Change leaf c: a and b must be recomputed (key depends on SCC closure),
	// while the number of transforms is exactly 3.
	src = NewMemorySource().
		Add("a", "alpha\n// @import b").
		Add("b", "beta\n// @import c").
		Add("c", "gamma-changed")
	r := buildOnce(t, src, cacheDir, outDir, OutputSingle, "a")
	if r.CacheHits != 0 || r.Transformed != 3 {
		t.Fatalf("upstream change: want 0 hits/3 xform, got hits=%d xform=%d", r.CacheHits, r.Transformed)
	}

	// Change nothing: everything hits.
	r = buildOnce(t, src, cacheDir, outDir, OutputSingle, "a")
	if r.CacheHits != 3 || r.Transformed != 0 {
		t.Fatalf("unchanged: want 3 hits, got %d/%d", r.CacheHits, r.Transformed)
	}

	// Change only b: b and a recomputed; c reuses cache.
	src = NewMemorySource().
		Add("a", "alpha\n// @import b").
		Add("b", "beta-v2\n// @import c").
		Add("c", "gamma-changed")
	r = buildOnce(t, src, cacheDir, outDir, OutputSingle, "a")
	if r.CacheHits != 1 || r.Transformed != 2 {
		t.Fatalf("b change: want 1 hit/2 xform, got hits=%d xform=%d", r.CacheHits, r.Transformed)
	}
}

func TestUnrelatedModuleNotInvalidated(t *testing.T) {
	dir := t.TempDir()
	cacheDir, outDir := filepath.Join(dir, "cache"), filepath.Join(dir, "out")
	src := NewMemorySource().
		Add("a", "alpha").
		Add("b", "beta")
	buildOnce(t, src, cacheDir, outDir, OutputSingle)

	src = NewMemorySource().
		Add("a", "alpha-v2").
		Add("b", "beta")
	r := buildOnce(t, src, cacheDir, outDir, OutputSingle)
	if r.CacheHits != 1 || r.Transformed != 1 {
		t.Fatalf("want only a recomputed, hits=%d xform=%d", r.CacheHits, r.Transformed)
	}
}

func TestDeleteDependencyDropsOldContent(t *testing.T) {
	dir := t.TempDir()
	cacheDir, outDir := filepath.Join(dir, "cache"), filepath.Join(dir, "out")
	src := NewMemorySource().
		Add("a", "alpha\n// @import b").
		Add("b", "SECRET-beta")
	r := buildOnce(t, src, cacheDir, outDir, OutputSingle, "a")
	if !strings.Contains(string(r.Artifacts[0].Content), "SECRET-beta") {
		t.Fatal("precondition: b content should be bundled")
	}

	// Delete b and the import edge.
	src = NewMemorySource().Add("a", "alpha-standalone")
	r = buildOnce(t, src, cacheDir, outDir, OutputSingle, "a")
	if strings.Contains(string(r.Artifacts[0].Content), "SECRET-beta") {
		t.Fatal("deleted dependency content still affects the artifact")
	}
	if strings.Contains(string(r.Artifacts[0].Content), "begin module: b ") {
		t.Fatal("deleted module id still appears in artifact")
	}
}

func TestRenameModule(t *testing.T) {
	dir := t.TempDir()
	cacheDir, outDir := filepath.Join(dir, "cache"), filepath.Join(dir, "out")
	src := NewMemorySource().Add("old/name", "x")
	buildOnce(t, src, cacheDir, outDir, OutputSingle)

	src = NewMemorySource().Add("new/name", "x")
	r := buildOnce(t, src, cacheDir, outDir, OutputSingle)
	if strings.Contains(string(r.Artifacts[0].Content), "old/name") {
		t.Fatal("renamed module: old id still present")
	}
	if !strings.Contains(string(r.Artifacts[0].Content), "new/name") {
		t.Fatal("renamed module: new id missing")
	}
}

func TestSplitModeDeterministic(t *testing.T) {
	dir := t.TempDir()
	src := NewMemorySource().
		Add("a/x", "ax\n// @import common").
		Add("a/y", "ay\n// @import common").
		Add("common", "shared")
	entries := []string{"a/y", "a/x"} // order must be ignored

	r1 := buildOnce(t, src, filepath.Join(dir, "c1"), filepath.Join(dir, "o1"), OutputSplit, entries...)
	r2 := buildOnce(t, src, filepath.Join(dir, "c2"), filepath.Join(dir, "o2"), OutputSplit, entries...)

	if len(r1.Artifacts) != 2 {
		t.Fatalf("want 2 split artifacts, got %d", len(r1.Artifacts))
	}
	for i := range r1.Artifacts {
		if r1.Artifacts[i].Name != r2.Artifacts[i].Name {
			t.Fatalf("names differ: %q vs %q", r1.Artifacts[i].Name, r2.Artifacts[i].Name)
		}
		if string(r1.Artifacts[i].Content) != string(r2.Artifacts[i].Content) {
			t.Fatalf("content differs for %s", r1.Artifacts[i].Name)
		}
	}
	names := []string{r1.Artifacts[0].Name, r1.Artifacts[1].Name}
	want := "entries/a_2fx.out,entries/a_2fy.out"
	if strings.Join(names, ",") != want {
		t.Fatalf("names = %v, want %s", names, want)
	}
	// Each closure is self contained and deterministic across caches.
	for _, a := range r1.Artifacts {
		if !strings.Contains(string(a.Content), "shared") {
			t.Fatalf("artifact %s missing common dep", a.Name)
		}
	}
	// File layout actually published under current/.
	cur := filepath.Join(filepath.Dir(r1.ReleaseDir), "..", "current")
	for _, a := range r1.Artifacts {
		p := filepath.Join(cur, filepath.FromSlash(a.Name))
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("published file missing: %v", err)
		}
		if string(data) != string(a.Content) {
			t.Fatalf("published bytes differ for %s", a.Name)
		}
	}
}

func TestTransformFailureIsolatesAndLocates(t *testing.T) {
	dir := t.TempDir()
	src := NewMemorySource().
		Add("ok", "fine").
		Add("bad", "will fail")
	b := New(Config{
		Source: src, CacheDir: filepath.Join(dir, "c"), OutputDir: filepath.Join(dir, "o"),
		Pipeline: NewPipeline(TrimTransform{}, failingTransform{module: "bad"}),
	})
	_, err := b.Build(context.Background())
	var se *StepError
	if !errors.As(err, &se) {
		t.Fatalf("want StepError, got %v", err)
	}
	if se.Module != "bad" || se.Step != 1 || se.Name != "fail" {
		t.Fatalf("step error location wrong: %+v", se)
	}
	// No half-finished release visible.
	if _, statErr := os.Lstat(filepath.Join(filepath.Join(dir, "o"), "current")); !os.IsNotExist(statErr) {
		t.Fatalf("current must not be published on failure, statErr=%v", statErr)
	}
}

type failingTransform struct{ module string }

func (failingTransform) Name() string { return "fail" }
func (f failingTransform) Apply(_ context.Context, in []byte) ([]byte, error) {
	if strings.Contains(string(in), "will fail") {
		return nil, errors.New("intentional transform failure")
	}
	return in, nil
}

func TestCycleBuildProducesExplainableOutput(t *testing.T) {
	dir := t.TempDir()
	src := NewMemorySource().
		Add("a", "A-body\n// @import b").
		Add("b", "B-body\n// @import a")
	r := buildOnce(t, src, filepath.Join(dir, "c"), filepath.Join(dir, "o"), OutputSingle)
	if len(r.Cycles) != 1 {
		t.Fatalf("want 1 cycle reported, got %d", len(r.Cycles))
	}
	if !strings.Contains(string(r.Artifacts[0].Content), "A-body") ||
		!strings.Contains(string(r.Artifacts[0].Content), "B-body") {
		t.Fatal("cycle members must both be bundled")
	}
	// Incremental with cycle: both hit; changing one invalidates both.
	r2 := buildOnce(t, src, filepath.Join(dir, "c"), filepath.Join(dir, "o"), OutputSingle)
	if r2.CacheHits != 2 {
		t.Fatalf("want 2 cache hits on cycle, got %d", r2.CacheHits)
	}
	src2 := NewMemorySource().
		Add("a", "A-v2\n// @import b").
		Add("b", "B-body\n// @import a")
	r3 := buildOnce(t, src2, filepath.Join(dir, "c"), filepath.Join(dir, "o"), OutputSingle)
	if r3.CacheHits != 0 || r3.Transformed != 2 {
		t.Fatalf("cycle edit must invalidate full SCC, got hits=%d xform=%d", r3.CacheHits, r3.Transformed)
	}
}
