// Package bundler 提供面向工作区源码的模块打包引擎：
// 依赖解析、转换流水线、内容寻址增量缓存、产物合成与持久化。
package bundler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// Source 描述一个输入模块的来源：本地文件或进程内生成的内容。
type Source struct {
	// Path 是模块在工作区内的逻辑路径（相对、正斜杠分隔），作为模块 ID。
	Path string
	// Content 为进程内提供的内容；为空时从 Root 下按 Path 读取文件。
	Content []byte
	// HasContent 为 true 时优先使用 Content，不读磁盘。
	HasContent bool
}

// DepStatus 区分依赖解析结果。
type DepStatus int

const (
	DepOK        DepStatus = iota // 正常解析
	DepMissing                    // 依赖目标不存在
	DepDuplicate                  // 同一模块被多处重复声明（已去重，仅记录）
)

// DepEdge 表示一条依赖边。
type DepEdge struct {
	Specifier string    // 源码中原始引用串
	Resolved  string    // 解析后的模块 ID；未解析成功时为空
	Status    DepStatus // 解析状态
}

// Module 是模块图中的一个节点。
type Module struct {
	ID      string    // 模块 ID（相对路径）
	Source  []byte    // 原始内容
	Deps    []DepEdge // 依赖边（按源码出现顺序）
	Hash    string    // 源内容哈希
	OutHash string    // 转换产物哈希（含依赖传递闭包），作为缓存键
	Output  []byte    // 转换后的内容
	Cyclic  bool      // 是否处于某个依赖环上
}

// Cycle 记录一个被确定化处理的依赖环。
type Cycle struct {
	// Members 为环上模块 ID，按字典序排序，保证可解释、可复现。
	Members []string
	// Entry 为环的入口（Members 中字典序最小者），合成时代码只在其处内联一次。
	Entry string
}

// Graph 是依赖解析的结果。
type Graph struct {
	Modules map[string]*Module
	Entries []string
	Cycles  []Cycle
	Missing []DepEdge // 汇总所有缺失依赖
	Topo    []string  // 拓扑序（依赖在前）；环按 Entry 收缩后参与排序
}

// TransformError 定位到模块与步骤的转换错误。
type TransformError struct {
	Module string
	Step   string
	Err    error
}

func (e *TransformError) Error() string {
	return fmt.Sprintf("transform failed: module=%s step=%s: %v", e.Module, e.Step, e.Err)
}

func (e *TransformError) Unwrap() error { return e.Err }

// Transform 是一个内容转换步骤。
type Transform interface {
	Name() string
	Apply(ctx context.Context, moduleID string, content []byte) ([]byte, error)
}

// TransformFunc 便于用函数构造 Transform。
type TransformFunc struct {
	StepName string
	Fn       func(ctx context.Context, moduleID string, content []byte) ([]byte, error)
}

func (t TransformFunc) Name() string { return t.StepName }

func (t TransformFunc) Apply(ctx context.Context, moduleID string, content []byte) ([]byte, error) {
	return t.Fn(ctx, moduleID, content)
}

// SplitMode 决定产物合成方式。
type SplitMode int

const (
	SplitNone     SplitMode = iota // 单一产物
	SplitPerEntry                  // 按入口拆分
)

// Config 是构建配置。
type Config struct {
	Root       string      // 工作区根目录（读取 Source.Path 对应文件）
	Entries    []string    // 入口模块 ID
	Transforms []Transform // 转换流水线，按固定顺序执行
	Split      SplitMode   // 产物拆分模式
	CacheDir   string      // 缓存目录；为空则仅内存缓存
	OutDir     string      // 产物输出目录；为空则不落盘
	Workers    int         // 并发度；<=0 时使用默认值
}

// Artifact 是一份产物。
type Artifact struct {
	Name    string // 产物文件名（确定性）
	Content []byte
}

// BuildResult 是一次构建的结果。
type BuildResult struct {
	Graph     *Graph
	Artifacts []Artifact
	// CacheHits / CacheMisses 记录缓存命中情况，便于测试与观测。
	CacheHits   []string
	CacheMisses []string
}

// hashBytes 计算内容的十六进制 SHA-256。
func hashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// sortedKeys 返回 map 键的排序副本，保证遍历确定性。
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// normalizePath 将 ./ ../ 等相对引用规整为正斜杠相对路径。
func normalizePath(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	parts := strings.Split(p, "/")
	var out []string
	for _, part := range parts {
		switch part {
		case "", ".":
			// 跳过
		case "..":
			if len(out) > 0 {
				out = out[:len(out)-1]
			}
		default:
			out = append(out, part)
		}
	}
	return strings.Join(out, "/")
}
