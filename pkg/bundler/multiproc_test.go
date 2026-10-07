package bundler

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestMultiProcessSharedCache runs several actual OS processes against one
// cache directory. flock must serialize index writes; each build must finish
// with a valid, self-consistent index and identical artifact bytes.
func TestMultiProcessSharedCache(t *testing.T) {
	if os.Getenv("BUNDLER_CHILD") == "1" {
		runChildBuild()
		return
	}
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")

	const procs = 4
	var wg sync.WaitGroup
	wg.Add(procs)
	start := make(chan struct{})
	for i := 0; i < procs; i++ {
		i := i
		go func() {
			defer wg.Done()
			outDir := filepath.Join(root, fmt.Sprintf("out-%d", i))
			cmd := exec.Command(os.Args[0], "-test.run", "TestMultiProcessSharedCache")
			cmd.Env = append(os.Environ(),
				"BUNDLER_CHILD=1",
				"BUNDLER_CACHE="+cacheDir,
				"BUNDLER_OUT="+outDir,
			)
			<-start
			var sb strings.Builder
			cmd.Stdout = &sb
			cmd.Stderr = &sb
			if err := cmd.Run(); err != nil {
				t.Errorf("child %d failed: %v\n%s", i, err, sb.String())
				return
			}
			var res childResult
			line := firstLine(sb.String())
			if err := json.Unmarshal([]byte(line), &res); err != nil {
				t.Errorf("child %d bad output: %v: %q", i, err, line)
				return
			}
			if !res.OK {
				t.Errorf("child %d reported failure", i)
			}
		}()
	}
	close(start)
	wg.Wait()

	// Final cache index must be a single valid JSON document with exactly
	// three module keys (never a torn write).
	data, err := os.ReadFile(filepath.Join(cacheDir, "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	var idx cacheIndex
	if err := json.Unmarshal(data, &idx); err != nil {
		t.Fatalf("index torn after multi-process build: %v", err)
	}
	if idx.FormatVersion != CacheFormatVersion || len(idx.Entries) != 3 {
		t.Fatalf("unexpected index: version=%d entries=%d", idx.FormatVersion, len(idx.Entries))
	}
	// Every referenced blob exists and a final build is fully cached.
	src := NewMemorySource().
		Add("a", "alpha\n// @import b").
		Add("b", "beta\n// @import c").
		Add("c", "gamma")
	cfg := Config{
		Source:    src,
		CacheDir:  cacheDir,
		OutputDir: filepath.Join(root, "final"),
		Pipeline:  NewPipeline(TrimTransform{}, BannerTransform{Text: "shared"}),
		Mode:      OutputSingle,
		Entries:   []string{"a"},
	}
	r, err := New(cfg).Build(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if r.CacheHits != 3 || r.Transformed != 0 {
		t.Fatalf("shared cache final build want 3 hits, got %d/%d", r.CacheHits, r.Transformed)
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return strings.TrimSpace(s)
}

type childResult struct {
	OK       bool   `json:"ok"`
	Artifact string `json:"artifact"`
	Error    string `json:"error,omitempty"`
}

func runChildBuild() {
	res := childResult{}
	defer func() {
		out, _ := json.Marshal(res)
		fmt.Println(string(out))
	}()
	src := NewMemorySource().
		Add("a", "alpha\n// @import b").
		Add("b", "beta\n// @import c").
		Add("c", "gamma")
	b := New(Config{
		Source:    src,
		CacheDir:  os.Getenv("BUNDLER_CACHE"),
		OutputDir: os.Getenv("BUNDLER_OUT"),
		Pipeline:  NewPipeline(TrimTransform{}, BannerTransform{Text: "shared"}),
		Mode:      OutputSingle,
		Entries:   []string{"a"},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r, err := b.Build(ctx)
	if err != nil {
		res.Error = err.Error()
		return
	}
	res.OK = true
	res.Artifact = string(r.Artifacts[0].Content)
}
