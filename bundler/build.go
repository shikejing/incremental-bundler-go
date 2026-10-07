package bundler

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Builder 是构建引擎入口，组合解析、转换、缓存与合成。
type Builder struct {
	cfg Config
}

// New 构造 Builder。
func New(cfg Config) *Builder { return &Builder{cfg: cfg} }

// transformFingerprint 标识转换流水线配置，纳入缓存键。
func transformFingerprint(transforms []Transform) string {
	names := make([]string, 0, len(transforms))
	for _, t := range transforms {
		names = append(names, t.Name())
	}
	return hashBytes([]byte(strings.Join(names, "|")))
}

// computeKeys 按拓扑序计算每个模块的缓存键 OutHash。
// 键 = 自身内容哈希 + 各依赖边的（状态, 解析结果, 依赖键）+ 流水线指纹。
// 任何上游内容或解析关系变化都会沿依赖边传导，使下游失效。
// 环成员共享一个环键：任一环成员或其外部依赖变化，整环失效。
func computeKeys(g *Graph, fp string) {
	cycleOf := map[string]string{}
	cycles := map[string]Cycle{}
	for _, c := range g.Cycles {
		cycles[c.Entry] = c
		for _, id := range c.Members {
			cycleOf[id] = c.Entry
		}
	}
	for _, id := range g.Topo {
		m := g.Modules[id]
		if entry, ok := cycleOf[id]; ok {
			if id != entry {
				continue // 环键在 Entry 处统一计算
			}
			cyc := cycles[entry]
			var sb strings.Builder
			sb.WriteString("cycle\x00" + fp)
			for _, member := range cyc.Members {
				mm := g.Modules[member]
				sb.WriteString("\x00" + member + "\x00" + mm.Hash)
				for _, e := range mm.Deps {
					if e.Status != DepOK || cycleOf[e.Resolved] == entry {
						continue
					}
					fmt.Fprintf(&sb, "\x00dep:%s:%s", e.Resolved, g.Modules[e.Resolved].OutHash)
				}
			}
			sccKey := hashBytes([]byte(sb.String()))
			for _, member := range cyc.Members {
				g.Modules[member].OutHash = hashBytes([]byte("cycmod\x00" + member + "\x00" + sccKey))
			}
			continue
		}
		var sb strings.Builder
		sb.WriteString("mod\x00" + fp + "\x00" + m.Hash)
		edges := append([]DepEdge(nil), m.Deps...)
		sort.Slice(edges, func(i, j int) bool { return edges[i].Specifier < edges[j].Specifier })
		for _, e := range edges {
			fmt.Fprintf(&sb, "\x00edge:%s:%d:%s", e.Specifier, e.Status, e.Resolved)
			if e.Status == DepOK {
				sb.WriteString(":" + g.Modules[e.Resolved].OutHash)
			}
		}
		m.OutHash = hashBytes([]byte(sb.String()))
	}
}

// Build 执行一次构建：解析 -> 计算缓存键 -> 并发转换（命中复用）-> 合成 -> 原子落盘。
// 任一模块转换失败不影响其他模块的转换与缓存提交；最终聚合返回错误且不产出产物。
// ctx 取消时已提交的缓存保持完整，产物不落盘，重新构建从最近一致位置继续。
func (b *Builder) Build(ctx context.Context, sources []Source) (*BuildResult, error) {
	resolver := NewResolver(b.cfg.Root, sources)
	g, err := resolver.Build(ctx, b.cfg.Entries)
	if err != nil {
		return nil, err
	}
	cache, err := OpenCache(b.cfg.CacheDir)
	if err != nil {
		return nil, err
	}
	fp := transformFingerprint(b.cfg.Transforms)
	computeKeys(g, fp)

	workers := b.cfg.Workers
	if workers <= 0 {
		workers = 8
	}
	sem := make(chan struct{}, workers)
	var mu sync.Mutex
	var hits, misses []string
	var errs []error
	var wg sync.WaitGroup

	for _, id := range g.Topo {
		m := g.Modules[id]
		wg.Add(1)
		go func(m *Module) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if err := ctx.Err(); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
				return
			}
			if out, ok, err := cache.Get(m.OutHash); err != nil {
				mu.Lock()
				errs = append(errs, fmt.Errorf("cache get %s: %w", m.ID, err))
				mu.Unlock()
				return
			} else if ok {
				m.Output = out
				mu.Lock()
				hits = append(hits, m.ID)
				mu.Unlock()
				return
			}
			out, err := runPipeline(ctx, b.cfg.Transforms, m.ID, m.Source)
			if err != nil {
				// 仅中止该模块；半成品不写入缓存与产物。
				mu.Lock()
				errs = append(errs, err)
				misses = append(misses, m.ID)
				mu.Unlock()
				return
			}
			if err := cache.Put(m.OutHash, out); err != nil {
				mu.Lock()
				errs = append(errs, fmt.Errorf("cache put %s: %w", m.ID, err))
				mu.Unlock()
				return
			}
			m.Output = out
			mu.Lock()
			misses = append(misses, m.ID)
			mu.Unlock()
		}(m)
	}
	wg.Wait()

	sort.Strings(hits)
	sort.Strings(misses)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}

	artifacts, err := emit(g, b.cfg.Split)
	if err != nil {
		return nil, err
	}
	if b.cfg.OutDir != "" {
		if err := writeArtifacts(b.cfg.OutDir, artifacts); err != nil {
			return nil, err
		}
	}
	return &BuildResult{
		Graph:       g,
		Artifacts:   artifacts,
		CacheHits:   hits,
		CacheMisses: misses,
	}, nil
}

// writeArtifacts 将产物原子落盘：先写临时文件再 rename；
// 依据 .manifest 清理上一轮产物，保证删除/改名后旧产物不残留。
// 任何时刻对外可见的都是完整一致的一组产物。
func writeArtifacts(outDir string, artifacts []Artifact) error {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	newNames := map[string]bool{}
	for _, a := range artifacts {
		newNames[a.Name] = true
		final := filepath.Join(outDir, a.Name)
		tmp, err := os.CreateTemp(outDir, ".tmp-*")
		if err != nil {
			return err
		}
		tmpName := tmp.Name()
		if _, err := tmp.Write(a.Content); err != nil {
			tmp.Close()
			os.Remove(tmpName)
			return err
		}
		if err := tmp.Sync(); err != nil {
			tmp.Close()
			os.Remove(tmpName)
			return err
		}
		if err := tmp.Close(); err != nil {
			os.Remove(tmpName)
			return err
		}
		if err := os.Rename(tmpName, final); err != nil {
			os.Remove(tmpName)
			return err
		}
	}
	// 读取上一轮清单，移除不再产出的旧产物。
	manifest := filepath.Join(outDir, ".manifest")
	if data, err := os.ReadFile(manifest); err == nil {
		for _, name := range strings.Fields(string(data)) {
			if !newNames[name] {
				_ = os.Remove(filepath.Join(outDir, name))
			}
		}
	}
	names := make([]string, 0, len(newNames))
	for n := range newNames {
		names = append(names, n)
	}
	sort.Strings(names)
	tmp := manifest + ".tmp"
	if err := os.WriteFile(tmp, []byte(strings.Join(names, "\n")+"\n"), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, manifest)
}
