package bundler

import (
	"context"
	"testing"
)

func src(path, content string) Source {
	return Source{Path: path, Content: []byte(content), HasContent: true}
}

func TestMissingDependencyIsRecorded(t *testing.T) {
	r := NewResolver("", []Source{
		src("a.js", `import "./b.js"; import "./ghost.js";`),
		src("b.js", `export const b = 1;`),
	})
	g, err := r.Build(context.Background(), []string{"a.js"})
	if err != nil {
		t.Fatalf("build graph: %v", err)
	}
	if len(g.Missing) != 1 || g.Missing[0].Specifier != "./ghost.js" {
		t.Fatalf("expected 1 missing dep for ./ghost.js, got %+v", g.Missing)
	}
	if g.Modules["a.js"].Deps[1].Status != DepMissing {
		t.Fatalf("edge status not DepMissing: %+v", g.Modules["a.js"].Deps[1])
	}
	if len(g.Modules) != 2 {
		t.Fatalf("expected 2 modules, got %d", len(g.Modules))
	}
}

func TestCycleDetectedAndDeterministic(t *testing.T) {
	sources := []Source{
		src("a.js", `import "./b.js";`),
		src("b.js", `import "./c.js";`),
		src("c.js", `import "./a.js";`),
		src("main.js", `import "./a.js";`),
	}
	// 重复构建两次，环记录与拓扑序必须一致。
	var prevTopo []string
	for i := 0; i < 2; i++ {
		g, err := NewResolver("", sources).Build(context.Background(), []string{"main.js"})
		if err != nil {
			t.Fatalf("build graph: %v", err)
		}
		if len(g.Cycles) != 1 {
			t.Fatalf("expected 1 cycle, got %v", g.Cycles)
		}
		cyc := g.Cycles[0]
		want := []string{"a.js", "b.js", "c.js"}
		if len(cyc.Members) != 3 || cyc.Entry != "a.js" {
			t.Fatalf("unexpected cycle: %+v", cyc)
		}
		for j, m := range cyc.Members {
			if m != want[j] {
				t.Fatalf("cycle members not sorted: %v", cyc.Members)
			}
			if !g.Modules[m].Cyclic {
				t.Fatalf("module %s not marked cyclic", m)
			}
		}
		if g.Modules["main.js"].Cyclic {
			t.Fatal("main.js wrongly marked cyclic")
		}
		// 拓扑序：环成员连续且 main.js 在最后。
		if len(g.Topo) != 4 || g.Topo[3] != "main.js" {
			t.Fatalf("unexpected topo order: %v", g.Topo)
		}
		if prevTopo != nil {
			for j := range prevTopo {
				if prevTopo[j] != g.Topo[j] {
					t.Fatalf("topo order not deterministic: %v vs %v", prevTopo, g.Topo)
				}
			}
		}
		prevTopo = g.Topo
	}
}

func TestSelfLoopIsCycle(t *testing.T) {
	g, err := NewResolver("", []Source{
		src("s.js", `import "./s.js";`),
	}).Build(context.Background(), []string{"s.js"})
	if err != nil {
		t.Fatalf("build graph: %v", err)
	}
	if len(g.Cycles) != 1 || g.Cycles[0].Entry != "s.js" {
		t.Fatalf("self loop not detected as cycle: %+v", g.Cycles)
	}
}

func TestDuplicateModuleDeduplicated(t *testing.T) {
	g, err := NewResolver("", []Source{
		src("a.js", `import "./shared.js";`),
		src("b.js", `import "./shared.js";`),
		src("shared.js", `export const s = 1;`),
	}).Build(context.Background(), []string{"a.js", "b.js"})
	if err != nil {
		t.Fatalf("build graph: %v", err)
	}
	if len(g.Modules) != 3 {
		t.Fatalf("duplicate module not deduplicated: %d modules", len(g.Modules))
	}
}

func TestDeepGraphDoesNotOverflowStack(t *testing.T) {
	// 构造超长依赖链，验证迭代式算法不耗尽栈。
	var sources []Source
	const n = 5000
	for i := 0; i < n; i++ {
		id := itoa(i) + ".js"
		next := itoa(i+1) + ".js"
		sources = append(sources, src(id, `import "./`+next+`";`))
	}
	sources = append(sources, src(itoa(n)+".js", `export const end = 1;`))
	g, err := NewResolver("", sources).Build(context.Background(), []string{"0.js"})
	if err != nil {
		t.Fatalf("build graph: %v", err)
	}
	if len(g.Modules) != n+1 {
		t.Fatalf("expected %d modules, got %d", n+1, len(g.Modules))
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [8]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}
