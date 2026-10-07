package bundler

import "strings"

// ModuleError locates a failure at a specific module.
type ModuleError struct {
	Module string
	Stage  string
	Err    error
}

func (e *ModuleError) Error() string {
	return "module " + e.Module + " at stage " + e.Stage + ": " + e.Err.Error()
}

func (e *ModuleError) Unwrap() error { return e.Err }

// StepError locates a transform failure at a module and pipeline step.
type StepError struct {
	Module string
	Step   int
	Name   string
	Err    error
}

func (e *StepError) Error() string {
	return "module " + e.Module + " transform step " + itoa(e.Step) +
		" (" + e.Name + ") failed: " + e.Err.Error()
}

func (e *StepError) Unwrap() error { return e.Err }

// DuplicateModuleError reports an ID supplied by more than one source file.
type DuplicateModuleError struct {
	ID      string
	Origins []string
}

func (e *DuplicateModuleError) Error() string {
	return "duplicate module " + e.ID + " defined in: " + strings.Join(e.Origins, ", ")
}

// MissingDependencyError reports an edge to an unknown module.
type MissingDependencyError struct {
	From string
	Dep  string
}

func (e *MissingDependencyError) Error() string {
	return "module " + e.From + " depends on missing module " + e.Dep
}

// BuildErrors aggregates independent module failures without one module's
// failure polluting the others.
type BuildErrors struct{ Errs []error }

func (e *BuildErrors) Error() string {
	parts := make([]string, len(e.Errs))
	for i, err := range e.Errs {
		parts[i] = err.Error()
	}
	return "build failed with " + itoa(len(e.Errs)) + " error(s):\n  - " +
		strings.Join(parts, "\n  - ")
}

func (e *BuildErrors) Unwrap() []error { return e.Errs }

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [20]byte
	p := len(b)
	for i > 0 {
		p--
		b[p] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		p--
		b[p] = '-'
	}
	return string(b[p:])
}
