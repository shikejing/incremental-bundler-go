package bundler

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// importPattern 匹配 ESM import ... from "x" 与 CJS require("x")。
var importPattern = regexp.MustCompile(`(?:import\s[^'"]*from\s*|import\s*|require\()\s*['"]([^'"]+)['"]`)

// Resolver 负责读取源码、解析依赖并构建模块图。
type Resolver struct {
	Root     string
	Provided map[string]Source // 进程内提供的内容，按模块 ID 索引
}

// NewResolver 构造解析器。sources 中 HasContent 的项优先于磁盘读取。
func NewResolver(root string, sources []Source) *Resolver {
	provided := make(map[string]Source, len(sources))
	for _, s := range sources {
		id := normalizePath(s.Path)
		s.Path = id
		provided[id] = s
	}
	return &Resolver{Root: root, Provided: provided}
}

// load 读取模块内容：优先进程内提供，其次磁盘。
func (r *Resolver) load(id string) ([]byte, bool, error) {
	if s, ok := r.Provided[id]; ok && s.HasContent {
		return s.Content, true, nil
	}
	if r.Root == "" {
		return nil, false, nil
	}
	full := filepath.Join(r.Root, filepath.FromSlash(id))
	data, err := os.ReadFile(full)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return data, true, nil
}

// resolveSpecifier 将引用串解析为模块 ID。
// 相对引用（./ ../）基于引用方所在目录；其余基于工作区根。
// 依次尝试：原样、追加 .js、追加 /index.js。
func (r *Resolver) resolveSpecifier(fromID, spec string) (string, bool, error) {
	var base string
	if strings.HasPrefix(spec, "./") || strings.HasPrefix(spec, "../") {
		dir := filepath.ToSlash(filepath.Dir(filepath.FromSlash(fromID)))
		if dir == "." {
			dir = ""
		}
		base = normalizePath(dir + "/" + spec)
	} else {
		base = normalizePath(spec)
	}
	candidates := []string{base, base + ".js", base + "/index.js"}
	for _, cand := range candidates {
		if _, ok := r.Provided[cand]; ok {
			return cand, true, nil
		}
		if r.Root != "" {
			st, err := os.Stat(filepath.Join(r.Root, filepath.FromSlash(cand)))
			if err == nil && !st.IsDir() {
				return cand, true, nil
			} else if err != nil && !os.IsNotExist(err) {
				return "", false, err
			}
		}
	}
	return "", false, nil
}

// parseDeps 从源码中提取依赖引用串（按出现顺序，去重）。
func parseDeps(src []byte) []string {
	var specs []string
	seen := map[string]bool{}
	for _, m := range importPattern.FindAllSubmatch(src, -1) {
		spec := string(m[1])
		if !seen[spec] {
			seen[spec] = true
			specs = append(specs, spec)
		}
	}
	return specs
}

// Build 从入口出发构建完整模块图。
// 缺失依赖记录为 DepMissing 并汇总到 Graph.Missing，不中断其他模块。
func (r *Resolver) Build(ctx context.Context, entries []string) (*Graph, error) {
	g := &Graph{Modules: map[string]*Module{}}
	queue := append([]string(nil), entries...)
	for len(queue) > 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		id := queue[0]
		queue = queue[1:]
		if _, ok := g.Modules[id]; ok {
			continue // 重复模块：去重，只保留首个
		}
		content, found, err := r.load(id)
		if err != nil {
			return nil, fmt.Errorf("load module %s: %w", id, err)
		}
		if !found {
			return nil, fmt.Errorf("entry module %q not found", id)
		}
		m := &Module{ID: id, Source: content, Hash: hashBytes(content)}
		g.Modules[id] = m
		for _, spec := range parseDeps(content) {
			resolved, ok, err := r.resolveSpecifier(id, spec)
			if err != nil {
				return nil, fmt.Errorf("resolve %q from %s: %w", spec, id, err)
			}
			edge := DepEdge{Specifier: spec, Resolved: resolved, Status: DepOK}
			if !ok {
				edge.Status = DepMissing
				g.Missing = append(g.Missing, edge)
			} else {
				queue = append(queue, resolved)
			}
			m.Deps = append(m.Deps, edge)
		}
	}
	g.Entries = append([]string(nil), entries...)
	detectCycles(g)
	g.Topo = topoOrder(g)
	return g, nil
}

// detectCycles 用 Tarjan SCC 找出所有依赖环，确定化记录到 Graph.Cycles。
// 环上模块标记 Cyclic；环的 Entry 为成员中字典序最小者。
func detectCycles(g *Graph) {
	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := map[string]int{}
	index := map[string]int{}
	low := map[string]int{}
	var stack []string
	onStack := map[string]bool{}
	counter := 0

	// 迭代式 Tarjan，避免深图耗尽栈。
	type frame struct {
		id   string
		next int // 下一个待访问的已解析依赖下标
	}
	ids := sortedKeys(g.Modules)
	for _, start := range ids {
		if color[start] != white {
			continue
		}
		var callStack []frame
		callStack = append(callStack, frame{id: start})
		color[start] = gray
		index[start] = counter
		low[start] = counter
		counter++
		stack = append(stack, start)
		onStack[start] = true
		for len(callStack) > 0 {
			f := &callStack[len(callStack)-1]
			m := g.Modules[f.id]
			advanced := false
			for f.next < len(m.Deps) {
				edge := m.Deps[f.next]
				f.next++
				if edge.Status != DepOK {
					continue
				}
				d := edge.Resolved
				if color[d] == white {
					color[d] = gray
					index[d] = counter
					low[d] = counter
					counter++
					stack = append(stack, d)
					onStack[d] = true
					callStack = append(callStack, frame{id: d})
					advanced = true
					break
				} else if onStack[d] {
					if index[d] < low[f.id] {
						low[f.id] = index[d]
					}
				}
			}
			if advanced {
				continue
			}
			// 弹出当前帧
			callStack = callStack[:len(callStack)-1]
			color[f.id] = black
			if len(callStack) > 0 {
				parent := callStack[len(callStack)-1].id
				if low[f.id] < low[parent] {
					low[parent] = low[f.id]
				}
			}
			if low[f.id] == index[f.id] {
				var scc []string
				for {
					top := stack[len(stack)-1]
					stack = stack[:len(stack)-1]
					onStack[top] = false
					scc = append(scc, top)
					if top == f.id {
						break
					}
				}
				isCycle := len(scc) > 1
				if !isCycle {
					// 自环也算环
					for _, e := range g.Modules[scc[0]].Deps {
						if e.Status == DepOK && e.Resolved == scc[0] {
							isCycle = true
							break
						}
					}
				}
				if isCycle {
					sort.Strings(scc)
					cyc := Cycle{Members: scc, Entry: scc[0]}
					g.Cycles = append(g.Cycles, cyc)
					for _, id := range scc {
						g.Modules[id].Cyclic = true
					}
				}
			}
		}
	}
	// 环列表按 Entry 排序，保证输出确定。
	sort.Slice(g.Cycles, func(i, j int) bool { return g.Cycles[i].Entry < g.Cycles[j].Entry })
}

// topoOrder 计算拓扑序（依赖在前）。环按 Entry 收缩：
// 环成员连续出现，Entry 在前，其余按字典序。整体结果确定可复现。
func topoOrder(g *Graph) []string {
	// 模块 -> 所属环 Entry
	cycleOf := map[string]string{}
	for _, c := range g.Cycles {
		for _, id := range c.Members {
			cycleOf[id] = c.Entry
		}
	}
	rep := func(id string) string {
		if e, ok := cycleOf[id]; ok {
			return e
		}
		return id
	}
	// 在压缩图上做 Kahn 拓扑排序（依赖在前），用字典序保证确定性。
	indeg := map[string]int{}
	adj := map[string][]string{} // dep -> dependents
	nodes := map[string]bool{}
	for id := range g.Modules {
		nodes[rep(id)] = true
	}
	for id, m := range g.Modules {
		ri := rep(id)
		for _, e := range m.Deps {
			if e.Status != DepOK {
				continue
			}
			rd := rep(e.Resolved)
			if rd == ri {
				continue // 环内边不参与压缩图
			}
			adj[rd] = append(adj[rd], ri)
			indeg[ri]++
		}
	}
	var avail []string
	for n := range nodes {
		if indeg[n] == 0 {
			avail = append(avail, n)
		}
	}
	sort.Strings(avail)
	var order []string
	membersOf := map[string][]string{}
	for _, c := range g.Cycles {
		membersOf[c.Entry] = c.Members
	}
	for len(avail) > 0 {
		n := avail[0]
		avail = avail[1:]
		if members, ok := membersOf[n]; ok {
			order = append(order, members...) // 已排序，Entry 为 members[0]
		} else {
			order = append(order, n)
		}
		nexts := append([]string(nil), adj[n]...)
		sort.Strings(nexts)
		for _, nxt := range nexts {
			indeg[nxt]--
			if indeg[nxt] == 0 {
				// 插入保持 avail 有序
				i := sort.SearchStrings(avail, nxt)
				avail = append(avail, "")
				copy(avail[i+1:], avail[i:])
				avail[i] = nxt
			}
		}
	}
	return order
}
