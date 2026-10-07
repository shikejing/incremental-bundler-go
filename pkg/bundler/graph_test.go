package bundler

import (
	"errors"
	"strings"
	"testing"
)

func memFiles(items ...[2]string) []SourceFile {
	var out []SourceFile
	for _, it := range items {
		out = append(out, SourceFile{ID: it[0], Content: []byte(it[1]), Origin: "memory:" + it[0]})
	}
	return out
}

func TestParseDeps(t *testing.T) {
	content := []byte("hello\n  // @import a/b\n//@import c\n// @import a/b\n// @import \n")
	got := ParseDeps(content)
	want := []string{"a/b", "c"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("deps = %v, want %v", got, want)
	}
}

func TestResolveDuplicateModules(t *testing.T) {
	files := []SourceFile{
		{ID: "a", Content: nil, Origin: "src/a.src"},
		{ID: "a", Content: nil, Origin: "src/other/a.src"},
	}
	_, _, err := Resolve(files, ResolveOptions{})
	var be *BuildErrors
	if !errors.As(err, &be) || len(be.Errs) != 1 {
		t.Fatalf("want one BuildErrors, got %v", err)
	}
	var dup *DuplicateModuleError
	if !errors.As(err, &dup) || dup.ID != "a" || len(dup.Origins) != 2 {
		t.Fatalf("want DuplicateModuleError with origins, got %v", err)
	}
}

func TestResolveMissingDependency(t *testing.T) {
	files := memFiles([2]string{"a", "// @import ghost"})

	// Default: fatal and locatable.
	_, _, err := Resolve(files, ResolveOptions{})
	var miss *MissingDependencyError
	if !errors.As(err, &miss) || miss.From != "a" || miss.Dep != "ghost" {
		t.Fatalf("want MissingDependencyError a->ghost, got %v", err)
	}

	// Stub policy: graph resolves, warning diagnostic emitted, stub exists.
	g, diags, err := Resolve(files, ResolveOptions{OnMissing: "stub"})
	if err != nil {
		t.Fatalf("stub policy should not fail: %v", err)
	}
	if _, ok := g.Modules["ghost"]; !ok {
		t.Fatal("stub module ghost not registered")
	}
	found := false
	for _, d := range diags {
		if d.Kind == "missing-stub" && d.Module == "a" {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing-stub diagnostic not emitted: %+v", diags)
	}
}

func TestResolveCycleDetectedAndExplained(t *testing.T) {
	files := memFiles(
		[2]string{"a", "// @import b\nA"},
		[2]string{"b", "// @import c\nB"},
		[2]string{"c", "// @import a\nC"},
	)
	g, diags, err := Resolve(files, ResolveOptions{OnMissing: "stub"})
	if err != nil {
		t.Fatalf("cycles must not be fatal: %v", err)
	}
	if len(g.Cycles) != 1 {
		t.Fatalf("want 1 cycle, got %d: %+v", len(g.Cycles), g.Cycles)
	}
	cy := g.Cycles[0]
	if strings.Join(cy.SCC, ",") != "a,b,c" {
		t.Fatalf("SCC = %v", cy.SCC)
	}
	if cy.Policy != "ascending-edges" || len(cy.CutEdges) == 0 {
		t.Fatalf("cycle policy/cuts not explained: %+v", cy)
	}
	// The exact deterministic cut: only c->a (non-ascending) is removed.
	if strings.Join(cy.CutEdges, ",") != "c -> a" {
		t.Fatalf("cuts = %v", cy.CutEdges)
	}
	// Edges are retained on modules (nothing dropped silently).
	if strings.Join(g.Modules["c"].Deps, ",") != "a" {
		t.Fatalf("declared deps rewritten: %v", g.Modules["c"].Deps)
	}
	if len(g.Order) != 3 {
		t.Fatalf("order = %v", g.Order)
	}
	// Build order keeps dependencies first along the surviving edges:
	// c < b < a.
	pos := map[string]int{}
	for i, id := range g.Order {
		pos[id] = i
	}
	if !(pos["c"] < pos["b"] && pos["b"] < pos["a"]) {
		t.Fatalf("unexpected order %v", g.Order)
	}
	found := false
	for _, d := range diags {
		if d.Kind == "cycle-broken" && strings.Contains(d.Message, "c -> a") {
			found = true
		}
	}
	if !found {
		t.Fatalf("cycle-broken diagnostic missing: %+v", diags)
	}
}

func TestResolveSelfLoop(t *testing.T) {
	files := memFiles([2]string{"a", "// @import a"})
	g, _, err := Resolve(files, ResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Cycles) != 1 || g.Cycles[0].CutEdges[0] != "a -> a" {
		t.Fatalf("self-loop not handled: %+v", g.Cycles)
	}
}

func TestResolveDeterministicTopoOrder(t *testing.T) {
	// Two independent roots and a diamond; ties must sort ascending.
	files := memFiles(
		[2]string{"d", "D"},
		[2]string{"c", "D"},
		[2]string{"b", "// @import d"},
		[2]string{"a", "// @import b\n// @import c"},
	)
	g, _, err := Resolve(files, ResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pos := map[string]int{}
	for i, id := range g.Order {
		pos[id] = i
	}
	if !(pos["d"] < pos["b"] && pos["c"] < pos["a"] && pos["b"] < pos["a"]) {
		t.Fatalf("bad order %v", g.Order)
	}
	// Two minimal nodes tie first: c < d ascending.
	if g.Order[0] != "c" {
		t.Fatalf("want c first, got %v", g.Order)
	}
}
