package bundler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Transform is one fixed-order content conversion step.
type Transform interface {
	Name() string
	Apply(ctx context.Context, in []byte) ([]byte, error)
}

// Pipeline is an ordered sequence of transforms.
type Pipeline struct {
	steps []Transform
}

func NewPipeline(steps ...Transform) *Pipeline {
	return &Pipeline{steps: append([]Transform(nil), steps...)}
}

func (p *Pipeline) Steps() []Transform { return append([]Transform(nil), p.steps...) }

// Hash identifies the exact pipeline configuration (step names and order).
// It feeds every cache key so changing the pipeline invalidates all modules.
func (p *Pipeline) Hash() string {
	h := sha256.New()
	for i, s := range p.steps {
		h.Write([]byte(itoa(i)))
		h.Write([]byte{0})
		h.Write([]byte(s.Name()))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// Run applies every step in order. A failure aborts the module immediately and
// the returned StepError locates the module and the failing step. No
// half-processed content is returned to the caller.
func (p *Pipeline) Run(ctx context.Context, module string, in []byte) ([]byte, error) {
	cur := in
	for i, s := range p.steps {
		if err := ctx.Err(); err != nil {
			return nil, &ModuleError{Module: module, Stage: "transform", Err: err}
		}
		out, err := s.Apply(ctx, cur)
		if err != nil {
			return nil, &StepError{
				Module: module,
				Step:   i,
				Name:   s.Name(),
				Err:    err,
			}
		}
		cur = out
	}
	return cur, nil
}

// TrimTransform removes leading/trailing whitespace from every line and drops
// empty lines. It is deterministic and never fails.
type TrimTransform struct{}

func (TrimTransform) Name() string { return "trim" }

func (TrimTransform) Apply(_ context.Context, in []byte) ([]byte, error) {
	lines := strings.Split(string(in), "\n")
	var b bytes.Buffer
	for _, ln := range lines {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		b.WriteString(ln)
		b.WriteByte('\n')
	}
	return b.Bytes(), nil
}

// BannerTransform prepends a fixed comment line. Deterministic: no timestamps.
type BannerTransform struct{ Text string }

func (b BannerTransform) Name() string { return "banner:" + b.Text }

func (b BannerTransform) Apply(_ context.Context, in []byte) ([]byte, error) {
	var out bytes.Buffer
	out.WriteString("// ")
	out.WriteString(b.Text)
	out.WriteByte('\n')
	out.Write(in)
	return out.Bytes(), nil
}
