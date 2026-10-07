package bundler

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// upper 是一个确定性的测试转换：内容转大写。
func upper() Transform {
	return TransformFunc{StepName: "upper", Fn: func(_ context.Context, _ string, c []byte) ([]byte, error) {
		return []byte(strings.ToUpper(string(c))), nil
	}}
}

func baseSources() []Source {
	return []Source{
		src("main.js", `import "./lib.js"; import "./util.js";`),
		src("lib.js", `import "./util.js"; export const lib = 1;`),
		src("util.js", `export const util = 1;`),
	}
}

func TestIncrementalInvalidation(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{Entries: []string{"main.js"}, Transforms: []Transform{upper()}, CacheDir: dir}
	b := New(cfg)

	r1, err := b.Build(context.Background(), baseSources())
	if err != nil {
		t.Fatalf("first build: %v", err)
	}
	if len(r1.CacheMisses) != 3 || len(r1.CacheHits) != 0 {
		t.Fatalf("first build should miss all: hits=%v misses=%v", r1.CacheHits, r1.CacheMisses)
	}

	// 第二次构建（新进程模拟：新建 Builder，同一缓存目录）：全部命中。
	r2, err := New(cfg).Build(context.Background(), baseSources())
	if err != nil {
		t.Fatalf("second build: %v", err)
	}
	if len(r2.CacheHits) != 3 || len(r2.CacheMisses) != 0 {
		t.Fatalf("second build should hit all: hits=%v misses=%v", r2.CacheHits, r2.CacheMisses)
	}
	if !bytes.Equal(r1.Artifacts[0].Content, r2.Artifacts[0].Content) {
		t.Fatal("incremental build output differs from full build")
	}

	// 修改 util.js：util 及其全部上游（lib, main）失效，其余复用。
	changed := baseSources()
	changed[2] = src("util.js", `export const util = 2;`)
	r3, err := New(cfg).Build(context.Background(), changed)
	if err != nil {
		t.Fatalf("third build: %v", err)
	}
	if len(r3.CacheHits) != 0 || len(r3.CacheMisses) != 3 {
		t.Fatalf("upstream change should invalidate dependents: hits=%v misses=%v", r3.CacheHits, r3.CacheMisses)
	}

	// 只改 main.js：仅 main 失效，lib/util 复用。
	onlyMain := baseSources()
	onlyMain[0] = src("main.js", `import "./lib.js"; import "./util.js"; export const main = 1;`)
	r4, err := New(cfg).Build(context.Background(), onlyMain)
	if err != nil {
		t.Fatalf("fourth build: %v", err)
	}
	if len(r4.CacheMisses) != 1 || r4.CacheMisses[0] != "main.js" || len(r4.CacheHits) != 2 {
		t.Fatalf("only main.js should be rebuilt: hits=%v misses=%v", r4.CacheHits, r4.CacheMisses)
	}
	// 与全量构建（空缓存）逐字节一致。
	rFull, err := New(Config{Entries: cfg.Entries, Transforms: cfg.Transforms, CacheDir: t.TempDir()}).Build(context.Background(), onlyMain)
	if err != nil {
		t.Fatalf("full build: %v", err)
	}
	if !bytes.Equal(r4.Artifacts[0].Content, rFull.Artifacts[0].Content) {
		t.Fatal("incremental and full build outputs differ")
	}
}

func TestTransformFailureIsolated(t *testing.T) {
	// 转换对含 "bad" 的内容失败；流水线配置保持不变，仅修复模块内容。
	bad := TransformFunc{StepName: "reject-bad", Fn: func(_ context.Context, id string, c []byte) ([]byte, error) {
		if bytes.Contains(c, []byte("bad")) {
			return nil, fmt.Errorf("boom")
		}
		return c, nil
	}}
	sources := baseSources()
	sources[1] = src("lib.js", `import "./util.js"; export const lib = "bad";`)
	cfg := Config{Entries: []string{"main.js"}, Transforms: []Transform{bad, upper()}, CacheDir: t.TempDir()}
	_, err := New(cfg).Build(context.Background(), sources)
	if err == nil {
		t.Fatal("expected error")
	}
	var te *TransformError
	if !errors.As(err, &te) {
		t.Fatalf("expected TransformError, got %v", err)
	}
	if te.Module != "lib.js" || te.Step != "reject-bad" {
		t.Fatalf("error not locatable: %+v", te)
	}
	// 修复 lib 内容后重构建：util 的缓存应已被提交并命中（失败未污染其他模块）。
	r, err := New(cfg).Build(context.Background(), baseSources())
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if len(r.CacheHits) != 1 || r.CacheHits[0] != "util.js" {
		t.Fatalf("util.js cache should survive lib.js failure: hits=%v", r.CacheHits)
	}
}

func TestAddDeleteRenameReflected(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{Entries: []string{"main.js"}, Transforms: []Transform{upper()}, CacheDir: dir}
	sources := []Source{
		src("main.js", `import "./old.js";`),
		src("old.js", `export const v = "old-content";`),
	}
	r1, err := New(cfg).Build(context.Background(), sources)
	if err != nil {
		t.Fatalf("build1: %v", err)
	}
	if !bytes.Contains(r1.Artifacts[0].Content, []byte("OLD-CONTENT")) {
		t.Fatal("old content missing from artifact")
	}
	// 删除 old.js，改名新增 new.js：旧内容不得继续影响产物。
	sources2 := []Source{
		src("main.js", `import "./new.js";`),
		src("new.js", `export const v = "new-content";`),
	}
	r2, err := New(cfg).Build(context.Background(), sources2)
	if err != nil {
		t.Fatalf("build2: %v", err)
	}
	if bytes.Contains(r2.Artifacts[0].Content, []byte("OLD-CONTENT")) {
		t.Fatal("deleted module content leaked into artifact")
	}
	if !bytes.Contains(r2.Artifacts[0].Content, []byte("NEW-CONTENT")) {
		t.Fatal("new module content missing from artifact")
	}
	if len(r2.Graph.Modules) != 2 {
		t.Fatalf("stale module in graph: %v", sortedKeys(r2.Graph.Modules))
	}
}

func TestSplitPerEntryDeterministic(t *testing.T) {
	sources := []Source{
		src("e1.js", `import "./shared.js";`),
		src("e2.js", `import "./shared.js";`),
		src("shared.js", `export const s = 1;`),
	}
	cfg := Config{Entries: []string{"e1.js", "e2.js"}, Transforms: []Transform{upper()}, Split: SplitPerEntry, CacheDir: t.TempDir()}
	r1, err := New(cfg).Build(context.Background(), sources)
	if err != nil {
		t.Fatalf("build1: %v", err)
	}
	if len(r1.Artifacts) != 2 {
		t.Fatalf("expected 2 artifacts, got %d", len(r1.Artifacts))
	}
	names := []string{r1.Artifacts[0].Name, r1.Artifacts[1].Name}
	if names[0] != "e1.bundle.js" || names[1] != "e2.bundle.js" {
		t.Fatalf("non-deterministic artifact names: %v", names)
	}
	// 重新构建（新缓存目录），产物必须逐字节一致。
	cfg.CacheDir = t.TempDir()
	r2, err := New(cfg).Build(context.Background(), sources)
	if err != nil {
		t.Fatalf("build2: %v", err)
	}
	for i := range r1.Artifacts {
		if r1.Artifacts[i].Name != r2.Artifacts[i].Name ||
			!bytes.Equal(r1.Artifacts[i].Content, r2.Artifacts[i].Content) {
			t.Fatal("split output not reproducible")
		}
	}
	// 每个入口产物只含自己的闭包。
	if !bytes.Contains(r1.Artifacts[0].Content, []byte("module: shared.js")) {
		t.Fatal("shared module missing from e1 bundle")
	}
	if bytes.Contains(r1.Artifacts[0].Content, []byte("module: e2.js")) {
		t.Fatal("e2 leaked into e1 bundle")
	}
}

func TestCycleBuildsAndExplains(t *testing.T) {
	sources := []Source{
		src("main.js", `import "./a.js";`),
		src("a.js", `import "./b.js";`),
		src("b.js", `import "./a.js";`),
	}
	cfg := Config{Entries: []string{"main.js"}, Transforms: []Transform{upper()}, CacheDir: t.TempDir()}
	r, err := New(cfg).Build(context.Background(), sources)
	if err != nil {
		t.Fatalf("build with cycle: %v", err)
	}
	if len(r.Graph.Cycles) != 1 {
		t.Fatalf("cycle not recorded: %+v", r.Graph.Cycles)
	}
	// 环成员在产物中各出现一次，且带可解释标注。
	c := r.Artifacts[0].Content
	if bytes.Count(c, []byte("module: a.js")) != 1 || bytes.Count(c, []byte("module: b.js")) != 1 {
		t.Fatal("cycle members should appear exactly once")
	}
	if !bytes.Contains(c, []byte("cyclic dependency group member")) {
		t.Fatal("cycle annotation missing from artifact")
	}
}

func TestWorkspaceFilesFromDisk(t *testing.T) {
	root := t.TempDir()
	writeFile := func(name, content string) {
		full := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeFile("src/main.js", `import "./lib/helper";`)
	writeFile("src/lib/helper.js", `export const h = 1;`)
	cfg := Config{Root: root, Entries: []string{"src/main.js"}, Transforms: []Transform{upper()}, CacheDir: t.TempDir()}
	r, err := New(cfg).Build(context.Background(), nil)
	if err != nil {
		t.Fatalf("build from disk: %v", err)
	}
	if len(r.Graph.Modules) != 2 || r.Graph.Modules["src/lib/helper.js"] == nil {
		t.Fatalf("disk modules not resolved: %v", sortedKeys(r.Graph.Modules))
	}
	if !bytes.Contains(r.Artifacts[0].Content, []byte("EXPORT CONST H = 1;")) {
		t.Fatal("transformed disk content missing from artifact")
	}
	// 删除磁盘文件后重建：模块消失，旧内容不再影响产物。
	if err := os.Remove(filepath.Join(root, "src/lib/helper.js")); err != nil {
		t.Fatal(err)
	}
	r2, err := New(cfg).Build(context.Background(), nil)
	if err != nil {
		t.Fatalf("rebuild after delete: %v", err)
	}
	if len(r2.Graph.Missing) != 1 {
		t.Fatalf("deleted file should become missing dep: %+v", r2.Graph.Missing)
	}
	if bytes.Contains(r2.Artifacts[0].Content, []byte("EXPORT CONST H = 1;")) {
		t.Fatal("deleted file content leaked into artifact")
	}
}
