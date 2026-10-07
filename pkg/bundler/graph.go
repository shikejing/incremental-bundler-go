package bundler

import "sort"

// Module is a resolved source module plus its declared dependency edges.
type Module struct {
	ID      string
	Content []byte
	Origin  string
	// Deps are the declared, de-duplicated dependency ids, in first-seen
	// order. They are never rewritten by cycle handling: cycles are broken
	// only for ordering, closure semantics keep every SCC member reachable.
	Deps []string
}

// Cycle explains one detected strongly-connected component and the
// deterministic edge-cut policy applied so the graph can be ordered.
type Cycle struct {
	// SCC are the member module ids, sorted ascending.
	SCC []string
	// Edges are all original intra-SCC dependency edges "from -> to", sorted.
	Edges []string
	// CutEdges are the edges removed for ordering only ("from -> to").
	CutEdges []string
	// Policy is the deterministic decision rule that produced CutEdges.
	Policy string
}

// Graph is the resolved module graph.
type Graph struct {
	Modules map[string]*Module
	// Order is the deterministic topological build order after cycle cuts.
	Order []string
	// Cycles explains every broken cycle; nothing is dropped silently.
	Cycles []Cycle

	// cutEdges marks edges removed for ordering: cutEdges[from][to] == true.
	cutEdges map[string]map[string]bool
	// sccOf maps module id to its SCC index; sccMembers lists members.
	sccOf      map[string]int
	sccMembers [][]string
}

// edge format helper
func edgeKey(from, to string) string { return from + " -> " + to }

// ParseDeps extracts dependency declarations from module content.
// Canonical form (one per line, leading whitespace allowed):
//
//	// @import other/module
//
// The compact form "//@import other/module" is also accepted. Declarations
// are de-duplicated in first-seen order; lines are matched verbatim and no
// transitive expansion happens here.
func ParseDeps(content []byte) []string {
	var deps []string
	seen := map[string]bool{}
	start := 0
	for i := 0; i <= len(content); i++ {
		if i < len(content) && content[i] != '\n' {
			continue
		}
		line := string(content[start:i])
		start = i + 1
		fields := splitFields(line)
		if len(fields) < 2 {
			continue
		}
		var id string
		switch {
		case len(fields) >= 3 && fields[0] == "//" && fields[1] == "@import":
			id = fields[2]
		case fields[0] == "//@import":
			id = fields[1]
		default:
			continue
		}
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		deps = append(deps, id)
	}
	return deps
}

// splitFields is strings.Fields without importing strings here.
func splitFields(s string) []string {
	var out []string
	i := 0
	for i < len(s) {
		for i < len(s) && isSpace(s[i]) {
			i++
		}
		j := i
		for j < len(s) && !isSpace(s[j]) {
			j++
		}
		if j > i {
			out = append(out, s[i:j])
		}
		i = j
	}
	return out
}

func isSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\r' || b == '\v' || b == '\f'
}

// ResolveOptions configures graph resolution.
type ResolveOptions struct {
	// OnMissing: "error" (default) aborts; "stub" synthesizes an empty
	// module for the missing id and emits a warning diagnostic.
	OnMissing string
}

// Resolve builds the module graph, distinguishing duplicate modules, missing
// dependencies and cycles.
//
// Duplicates are fatal (BuildErrors of DuplicateModuleError). Missing
// dependencies are either fatal or replaced by explained stubs. Cycles are
// never fatal: SCCs are found with an iterative Tarjan search (bounded stack),
// then broken by a deterministic policy and reported via Cycle records plus a
// "cycle-broken" diagnostic.
func Resolve(files []SourceFile, opts ResolveOptions) (*Graph, []Diagnostic, error) {
	// 1. Register modules, detecting duplicate ids.
	mods := map[string]*Module{}
	origins := map[string][]string{}
	var dupIDs []string
	for _, f := range files {
		if _, ok := mods[f.ID]; ok {
			if len(origins[f.ID]) == 1 {
				dupIDs = append(dupIDs, f.ID)
			}
			origins[f.ID] = append(origins[f.ID], f.Origin)
			continue
		}
		origins[f.ID] = append(origins[f.ID], f.Origin)
		content := make([]byte, len(f.Content))
		copy(content, f.Content)
		mods[f.ID] = &Module{
			ID:      f.ID,
			Content: content,
			Origin:  f.Origin,
			Deps:    ParseDeps(f.Content),
		}
	}
	if len(dupIDs) > 0 {
		sort.Strings(dupIDs)
		errs := make([]error, 0, len(dupIDs))
		for _, id := range dupIDs {
			errs = append(errs, &DuplicateModuleError{ID: id, Origins: origins[id]})
		}
		return nil, nil, &BuildErrors{Errs: errs}
	}

	var diags []Diagnostic

	// 2. Resolve missing dependency edges.
	missing := map[string]bool{}
	for _, m := range mods {
		for _, dep := range m.Deps {
			if _, ok := mods[dep]; !ok {
				missing[dep+"\x00"+m.ID] = true
			}
		}
	}
	if len(missing) > 0 {
		keys := make([]string, 0, len(missing))
		for k := range missing {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if opts.OnMissing != "stub" {
			errs := make([]error, 0, len(keys))
			for _, k := range keys {
				dep, from := splitMissingKey(k)
				errs = append(errs, &MissingDependencyError{From: from, Dep: dep})
			}
			return nil, nil, &BuildErrors{Errs: errs}
		}
		stubCreated := map[string]bool{}
		for _, k := range keys {
			dep, from := splitMissingKey(k)
			diags = append(diags, Diagnostic{
				Severity: SeverityWarning,
				Module:   from,
				Kind:     "missing-stub",
				Message:  "dependency " + dep + " not found; using empty stub module",
				Detail:   map[string]string{"missing": dep, "from": from},
			})
			if !stubCreated[dep] {
				stubCreated[dep] = true
				mods[dep] = &Module{
					ID:      dep,
					Content: nil,
					Origin:  "stub:" + dep,
				}
			}
		}
	}

	g := &Graph{Modules: mods, cutEdges: map[string]map[string]bool{}, sccOf: map[string]int{}}

	// 3. Iterative Tarjan SCC (explicit work stack; no recursion depth risk).
	g.sccMembers = tarjanSCC(mods)
	for i, members := range g.sccMembers {
		for _, id := range members {
			g.sccOf[id] = i
		}
	}

	// 4. Deterministic cycle cutting.
	// Policy "ascending-edges": members of an SCC are sorted ascending; only
	// edges from a smaller id to a larger id survive. Edges u->v with u >= v
	// (including self-loops) are cut for ordering purposes. SCC reachability
	// (hence bundle closure) is unaffected because every member still
	// reaches every other member through the original declared edges.
	const policy = "ascending-edges"
	var cycleIDs []int
	for i, members := range g.sccMembers {
		if len(members) > 1 || hasSelfLoop(mods, members[0]) {
			cycleIDs = append(cycleIDs, i)
		}
	}
	sort.Slice(cycleIDs, func(a, b int) bool {
		return g.sccMembers[cycleIDs[a]][0] < g.sccMembers[cycleIDs[b]][0]
	})
	for _, ci := range cycleIDs {
		members := append([]string(nil), g.sccMembers[ci]...)
		sort.Strings(members)
		inSCC := map[string]bool{}
		for _, id := range members {
			inSCC[id] = true
		}
		var edges, cut []string
		for _, from := range members {
			for _, to := range mods[from].Deps {
				if !inSCC[to] {
					continue
				}
				edges = append(edges, edgeKey(from, to))
				if from >= to { // cut non-ascending and self-loop edges
					cut = append(cut, edgeKey(from, to))
					if g.cutEdges[from] == nil {
						g.cutEdges[from] = map[string]bool{}
					}
					g.cutEdges[from][to] = true
				}
			}
		}
		sort.Strings(edges)
		sort.Strings(cut)
		g.Cycles = append(g.Cycles, Cycle{
			SCC: members, Edges: edges, CutEdges: cut, Policy: policy,
		})
		diags = append(diags, Diagnostic{
			Severity: SeverityWarning,
			Kind:     "cycle-broken",
			Message:  "cycle detected among [" + joinComma(members) + "]; cut edges: [" + joinComma(cut) + "]",
			Detail: map[string]string{
				"policy": policy,
				"scc":    joinComma(members),
				"cut":    joinComma(cut),
			},
		})
	}

	// 5. Deterministic Kahn topological order, ties broken by id ascending.
	g.Order = topoOrder(mods, g.cutEdges)
	return g, diags, nil
}

func splitMissingKey(k string) (dep, from string) {
	for i := 0; i < len(k); i++ {
		if k[i] == 0 {
			return k[:i], k[i+1:]
		}
	}
	return k, ""
}

func hasSelfLoop(mods map[string]*Module, id string) bool {
	for _, dep := range mods[id].Deps {
		if dep == id {
			return true
		}
	}
	return false
}

func joinComma(xs []string) string {
	out := ""
	for i, x := range xs {
		if i > 0 {
			out += ", "
		}
		out += x
	}
	return out
}

// tarjanFrame is one paused strong-connect invocation.
type tarjanFrame struct {
	v  string
	pi int // next neighbor index
}

func tarjanSCC(mods map[string]*Module) [][]string {
	ids := make([]string, 0, len(mods))
	for id := range mods {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	adj := make(map[string][]string, len(mods))
	for _, id := range ids {
		deps := append([]string(nil), mods[id].Deps...)
		sort.Strings(deps)
		adj[id] = deps
	}

	indexOf := map[string]int{}
	low := map[string]int{}
	onStack := map[string]bool{}
	var stack []string
	next := 0
	var sccs [][]string

	for _, root := range ids {
		if _, done := indexOf[root]; done {
			continue
		}
		indexOf[root] = next
		low[root] = next
		next++
		stack = append(stack, root)
		onStack[root] = true
		work := []tarjanFrame{{v: root}}

		for len(work) > 0 {
			frame := &work[len(work)-1]
			v := frame.v
			if frame.pi < len(adj[v]) {
				w := adj[v][frame.pi]
				frame.pi++
				if _, seen := indexOf[w]; !seen {
					indexOf[w] = next
					low[w] = next
					next++
					stack = append(stack, w)
					onStack[w] = true
					work = append(work, tarjanFrame{v: w})
					continue
				}
				if onStack[w] {
					if indexOf[w] < low[v] {
						low[v] = indexOf[w]
					}
				}
				continue
			}
			// Finished v: close an SCC if v is a root.
			if low[v] == indexOf[v] {
				var scc []string
				for {
					w := stack[len(stack)-1]
					stack = stack[:len(stack)-1]
					onStack[w] = false
					scc = append(scc, w)
					if w == v {
						break
					}
				}
				sccs = append(sccs, scc)
			}
			work = work[:len(work)-1]
			if len(work) > 0 {
				p := work[len(work)-1].v
				if low[v] < low[p] {
					low[p] = low[v]
				}
			}
		}
	}
	return sccs
}

func topoOrder(mods map[string]*Module, cut map[string]map[string]bool) []string {
	indeg := make(map[string]int, len(mods))
	successors := make(map[string][]string, len(mods))
	for id, m := range mods {
		indeg[id] = 0
		_ = m
	}
	for from, m := range mods {
		for _, to := range m.Deps {
			if cut[from] != nil && cut[from][to] {
				continue
			}
			indeg[from]++
			successors[to] = append(successors[to], from)
		}
	}
	// ready as sorted slice (small module graphs; deterministic min selection).
	var ready []string
	for id, d := range indeg {
		if d == 0 {
			ready = append(ready, id)
		}
	}
	sort.Strings(ready)
	var order []string
	for len(ready) > 0 {
		v := ready[0]
		ready = ready[1:]
		order = append(order, v)
		succ := append([]string(nil), successors[v]...)
		sort.Strings(succ)
		for _, w := range succ {
			indeg[w]--
			if indeg[w] == 0 {
				ready = insertSorted(ready, w)
			}
		}
	}
	return order
}

func insertSorted(xs []string, x string) []string {
	i := sort.SearchStrings(xs, x)
	xs = append(xs, "")
	copy(xs[i+1:], xs[i:])
	xs[i] = x
	return xs
}
