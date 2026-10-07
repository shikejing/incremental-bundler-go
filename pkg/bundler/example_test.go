package bundler_test

import (
	"context"
	"fmt"
	"os"

	bundler "github.com/shikejing/incremental-bundler-go/pkg/bundler"
)

// Example shows a full incremental build: resolve -> transform -> synthesize,
// then a second build that fully reuses the persistent cache.
func Example() {
	dir, err := os.MkdirTemp("", "bundler-example-")
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	defer os.RemoveAll(dir)

	src := bundler.NewMemorySource().
		Add("app/main", "// @import shared/util\nfunc main() {}").
		Add("shared/util", "func util() {}")

	cfg := bundler.Config{
		Source:    src,
		CacheDir:  dir + "/cache",
		OutputDir: dir + "/dist",
		Pipeline:  bundler.NewPipeline(bundler.TrimTransform{}),
		Entries:   []string{"app/main"},
		Mode:      bundler.OutputSingle,
	}

	r1, err := bundler.New(cfg).Build(context.Background())
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Println("first build artifacts:", len(r1.Artifacts), "transformed:", r1.Transformed)

	r2, err := bundler.New(cfg).Build(context.Background())
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Println("second build hits:", r2.CacheHits, "identical:",
		string(r1.Artifacts[0].Content) == string(r2.Artifacts[0].Content))
	// Output:
	// first build artifacts: 1 transformed: 2
	// second build hits: 2 identical: true
}
