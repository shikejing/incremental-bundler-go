package bundler

// Severity classifies a Diagnostic.
type Severity int

const (
	SeverityError Severity = iota
	SeverityWarning
)

// Diagnostic is an explainable, locatable build finding that does not abort
// the build (e.g. a cycle that was broken deterministically).
type Diagnostic struct {
	Severity Severity
	Module   string
	Kind     string
	Message  string
	Detail   map[string]string
}
