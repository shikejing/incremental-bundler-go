package bundler

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// SourceFile is a single input module read from a Source.
type SourceFile struct {
	ID      string
	Content []byte
	Origin  string
}

// Source provides workspace source files to the builder.
type Source interface {
	ReadAll(ctx context.Context) ([]SourceFile, error)
}

// MemorySource is an in-process Source. Adding the same ID more than once
// makes duplicates observable to the resolver instead of hiding them.
type MemorySource struct {
	files []SourceFile
}

func NewMemorySource() *MemorySource { return &MemorySource{} }

// Add registers one module; the string content is copied verbatim as bytes.
func (m *MemorySource) Add(id string, content string) *MemorySource {
	m.files = append(m.files, SourceFile{
		ID:      id,
		Content: []byte(content),
		Origin:  "memory:" + id,
	})
	return m
}

func (m *MemorySource) ReadAll(ctx context.Context) ([]SourceFile, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := make([]SourceFile, len(m.files))
	copy(out, m.files)
	return out, nil
}

// DirSource reads *.src files from a workspace root. The module ID is the
// slash-separated path relative to root, without the ".src" suffix, e.g.
// root/pkg/a.src -> "pkg/a".
type DirSource struct {
	root string
}

func NewDirSource(root string) *DirSource { return &DirSource{root: root} }

func (d *DirSource) ReadAll(ctx context.Context) ([]SourceFile, error) {
	root := filepath.Clean(d.root)
	var out []SourceFile
	err := filepath.WalkDir(root, func(path string, de os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if de.IsDir() {
			name := de.Name()
			if path != root && (strings.HasPrefix(name, ".") || name == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".src") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		id := filepath.ToSlash(strings.TrimSuffix(rel, ".src"))
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out = append(out, SourceFile{ID: id, Content: data, Origin: path})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read workspace %q: %w", root, err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}
