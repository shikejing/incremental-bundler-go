package bundler

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
)

// emit 将模块输出按拓扑序合成为产物。
// SplitNone：全部模块合成单一产物 bundle.js。
// SplitPerEntry：每个入口一个产物，包含其传递闭包内的模块。
// 同一输入同一配置下产物内容与命名确定可复现。
func emit(g *Graph, mode SplitMode) ([]Artifact, error) {
	switch mode {
	case SplitPerEntry:
		return emitPerEntry(g)
	default:
		return emitSingle(g)
	}
}

// renderModule 渲染单个模块，带确定性分隔头；环成员标注所属环。
func renderModule(m *Module) []byte {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "// ---- module: %s ----\n", m.ID)
	if m.Cyclic {
		buf.WriteString("// (cyclic dependency group member)\n")
	}
	buf.Write(m.Output)
	if len(m.Output) > 0 && m.Output[len(m.Output)-1] != '\n' {
		buf.WriteByte('\n')
	}
	buf.WriteByte('\n')
	return buf.Bytes()
}

func emitSingle(g *Graph) ([]Artifact, error) {
	var buf bytes.Buffer
	for _, id := range g.Topo {
		buf.Write(renderModule(g.Modules[id]))
	}
	return []Artifact{{Name: "bundle.js", Content: buf.Bytes()}}, nil
}

// reachable 计算入口的传递闭包（仅含已解析依赖）。
func reachable(g *Graph, entry string) map[string]bool {
	seen := map[string]bool{}
	var walk func(id string)
	walk = func(id string) {
		if seen[id] {
			return
		}
		seen[id] = true
		for _, e := range g.Modules[id].Deps {
			if e.Status == DepOK {
				walk(e.Resolved)
			}
		}
	}
	walk(entry)
	return seen
}

// entryArtifactName 由入口 ID 派生确定的产物名。
func entryArtifactName(entry string) string {
	name := strings.ReplaceAll(entry, "/", "_")
	name = strings.TrimSuffix(name, ".js")
	return name + ".bundle.js"
}

func emitPerEntry(g *Graph) ([]Artifact, error) {
	entries := append([]string(nil), g.Entries...)
	sort.Strings(entries)
	var arts []Artifact
	for _, entry := range entries {
		set := reachable(g, entry)
		var buf bytes.Buffer
		for _, id := range g.Topo {
			if set[id] {
				buf.Write(renderModule(g.Modules[id]))
			}
		}
		arts = append(arts, Artifact{Name: entryArtifactName(entry), Content: buf.Bytes()})
	}
	return arts, nil
}
