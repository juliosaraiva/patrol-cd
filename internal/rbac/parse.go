package rbac

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"
)

// Default input limits. Policies come from administrators (patrol-rbac-cm) and, for
// project roles, from project editors, so sizes are bounded.
const (
	// DefaultMaxPolicyBytes bounds the total size of all sources parsed together.
	DefaultMaxPolicyBytes = 16 << 20
	// DefaultMaxLineBytes bounds a single policy line (after trimming).
	DefaultMaxLineBytes = 16 << 10
)

// ErrInvalidPolicy is matched (errors.Is) by every error reporting a policy that cannot
// be loaded. The concrete type is *PolicyError.
var ErrInvalidPolicy = errors.New("invalid RBAC policy")

// Severity classifies a ValidationError.
type Severity string

const (
	// SeverityError marks a problem that prevents the policy from loading (upstream
	// rejects the same input).
	SeverityError Severity = "error"
	// SeverityWarning marks a line that loads but is probably not what the author meant
	// (for example a rule that can never match).
	SeverityWarning Severity = "warning"
)

// ValidationError describes one problem found in a policy source.
type ValidationError struct {
	// Source names the policy document (for example "policy.csv", "builtin" or
	// "project/<name>/role/<role>").
	Source string
	// Line is the 1-based line number within Source, or 0 when the problem is not tied to
	// a line.
	Line int
	// Text is the offending line with surrounding whitespace removed.
	Text string
	// Severity is SeverityError or SeverityWarning.
	Severity Severity
	// Message explains the problem.
	Message string
}

// Error implements error.
func (v ValidationError) Error() string {
	var b strings.Builder
	if v.Source != "" {
		b.WriteString(v.Source)
		if v.Line > 0 {
			fmt.Fprintf(&b, ":%d", v.Line)
		}
		b.WriteString(": ")
	}
	b.WriteString(string(v.Severity))
	b.WriteString(": ")
	b.WriteString(v.Message)
	if v.Text != "" {
		fmt.Fprintf(&b, " (line %q)", v.Text)
	}
	return b.String()
}

// PolicyError reports a policy that cannot be loaded. It matches ErrInvalidPolicy.
type PolicyError struct {
	// Problems holds the error-severity problems, in source order.
	Problems []ValidationError
}

// Error implements error.
func (e *PolicyError) Error() string {
	switch len(e.Problems) {
	case 0:
		return ErrInvalidPolicy.Error()
	case 1:
		return fmt.Sprintf("%s: %s", ErrInvalidPolicy, e.Problems[0].Error())
	default:
		return fmt.Sprintf("%s: %s (and %d more errors)", ErrInvalidPolicy, e.Problems[0].Error(), len(e.Problems)-1)
	}
}

// Unwrap makes errors.Is(err, ErrInvalidPolicy) true.
func (e *PolicyError) Unwrap() error { return ErrInvalidPolicy }

// HasErrors reports whether problems contains an error-severity entry.
func HasErrors(problems []ValidationError) bool {
	for _, p := range problems {
		if p.Severity == SeverityError {
			return true
		}
	}
	return false
}

// policyErrorFrom returns a *PolicyError holding the error-severity problems, or nil.
func policyErrorFrom(problems []ValidationError) error {
	var errs []ValidationError
	for _, p := range problems {
		if p.Severity == SeverityError {
			errs = append(errs, p)
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return &PolicyError{Problems: errs}
}

// Source is one named policy CSV document. Sources parsed together behave exactly like
// their texts concatenated with newlines (the ConfigMap composition rule), but problems
// keep per-source line numbers.
type Source struct {
	Name string
	CSV  string
	// Builtin marks the built-in policy. A pattern that cannot be evaluated in the
	// active match mode (an invalid regular expression, or a glob alternation without an
	// exact RE2 equivalent) is a warning in a built-in source, and any evaluation that
	// reaches it is denied, as upstream: the built-in policy is written for glob mode, so
	// its '*' patterns are invalid in regex mode. In every other source such a pattern
	// is an error, so the policy does not load and no evaluation can fail.
	Builtin bool
}

// ParseOptions configures Parse and ParseSources.
type ParseOptions struct {
	// MatchMode is "glob" (default) or "regex". Any other value selects glob, like
	// upstream, and is reported as a warning.
	MatchMode string
	// Source names the document for Parse; defaults to "policy".
	Source string
	// MaxBytes bounds the total input size; <= 0 means DefaultMaxPolicyBytes.
	MaxBytes int
	// MaxLineBytes bounds one trimmed line; <= 0 means DefaultMaxLineBytes.
	MaxLineBytes int
}

func (o ParseOptions) limits() (maxBytes, maxLine int) {
	maxBytes, maxLine = o.MaxBytes, o.MaxLineBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxPolicyBytes
	}
	if maxLine <= 0 {
		maxLine = DefaultMaxLineBytes
	}
	return maxBytes, maxLine
}

// Parse parses and compiles one policy CSV document. It returns every problem found
// (errors and warnings). When any error-severity problem exists the returned Policy is
// nil and err is a *PolicyError (errors.Is(err, ErrInvalidPolicy)).
func Parse(csvText string, opts ParseOptions) (*Policy, []ValidationError, error) {
	name := opts.Source
	if name == "" {
		name = "policy"
	}
	return ParseSources([]Source{{Name: name, CSV: csvText}}, opts)
}

// ParseSources parses several documents into one Policy, in order. See Parse.
func ParseSources(sources []Source, opts ParseOptions) (*Policy, []ValidationError, error) {
	mode, known := normalizeMatchMode(opts.MatchMode)
	maxBytes, maxLine := opts.limits()

	var problems []ValidationError
	if !known {
		problems = append(problems, ValidationError{
			Severity: SeverityWarning,
			Message:  fmt.Sprintf("unknown match mode %q; using %q", opts.MatchMode, GlobMatchMode),
		})
	}

	total := 0
	for _, s := range sources {
		total += len(s.CSV)
	}
	if total > maxBytes {
		problems = append(problems, ValidationError{
			Severity: SeverityError,
			Message:  fmt.Sprintf("policy is %d bytes, exceeding the limit of %d bytes", total, maxBytes),
		})
		return nil, problems, policyErrorFrom(problems)
	}

	b := newBuilder(mode)
	for _, s := range sources {
		lineNo := 0
		for raw := range strings.SplitSeq(s.CSV, "\n") {
			lineNo++
			b.addLine(s, lineNo, raw, maxLine, &problems)
		}
	}
	if err := policyErrorFrom(problems); err != nil {
		return nil, problems, err
	}
	return b.build(), problems, nil
}

// lineKind is the first field of a policy line.
type lineKind uint8

const (
	lineSkip   lineKind = iota // blank or comment
	linePolicy                 // p
	lineGroup                  // g
	lineNoop                   // r, e or m: accepted upstream, no effect
)

// parsedLine is a tokenized policy line.
type parsedLine struct {
	kind   lineKind
	tokens []string
	text   string
}

// tokenizeLine applies the upstream line grammar to one raw line: trim, skip blanks and
// comments, CSV-split with leading-space trimming, and check the line type and arity.
func tokenizeLine(raw string, maxLine int) (parsedLine, error) {
	text := strings.TrimSpace(raw)
	if text == "" || strings.HasPrefix(text, "#") {
		return parsedLine{kind: lineSkip}, nil
	}
	pl := parsedLine{text: text}
	if len(text) > maxLine {
		return pl, fmt.Errorf("line is %d bytes, exceeding the limit of %d bytes", len(text), maxLine)
	}
	tokens, err := splitCSVLine(text)
	if err != nil {
		return pl, err
	}
	pl.tokens = tokens
	switch tokens[0] {
	case "p":
		if len(tokens) != 6 {
			return pl, fmt.Errorf("'p' lines need 6 fields (p, subject, resource, action, object, effect), got %d", len(tokens))
		}
		pl.kind = linePolicy
	case "g":
		if len(tokens) != 3 {
			return pl, fmt.Errorf("'g' lines need 3 fields (g, member, role), got %d", len(tokens))
		}
		pl.kind = lineGroup
	case "r", "e", "m":
		pl.kind = lineNoop
	case "":
		return pl, errors.New("missing line type: the first field must be 'p' or 'g'")
	default:
		return pl, fmt.Errorf("unknown line type %q: the first field must be 'p' or 'g'", tokens[0])
	}
	return pl, nil
}

// splitCSVLine splits one trimmed, non-empty line into fields with the upstream CSV
// settings (encoding/csv, TrimLeadingSpace). Lines without quotes take an equivalent
// fast path: split on ',' and trim leading white space of each field.
func splitCSVLine(text string) ([]string, error) {
	if !strings.Contains(text, `"`) {
		fields := strings.Split(text, ",")
		for i, f := range fields {
			fields[i] = strings.TrimLeftFunc(f, unicode.IsSpace)
		}
		return fields, nil
	}
	r := csv.NewReader(strings.NewReader(text))
	r.TrimLeadingSpace = true
	tokens, err := r.Read()
	if err != nil {
		var pe *csv.ParseError
		if errors.As(err, &pe) {
			return nil, fmt.Errorf("cannot parse CSV at column %d: %w", pe.Column, pe.Err)
		}
		if errors.Is(err, io.EOF) {
			return nil, errors.New("cannot parse CSV: empty record")
		}
		return nil, fmt.Errorf("cannot parse CSV: %w", err)
	}
	return tokens, nil
}

// addLine parses one raw line of src and adds its rule or link to the builder,
// appending problems.
func (b *builder) addLine(src Source, lineNo int, raw string, maxLine int, problems *[]ValidationError) {
	source := src.Name
	pl, err := tokenizeLine(raw, maxLine)
	if err != nil {
		*problems = append(*problems, ValidationError{Source: source, Line: lineNo, Text: pl.text, Severity: SeverityError, Message: err.Error()})
		return
	}
	loc := location{source: source, line: lineNo, text: pl.text}
	warn := func(format string, args ...any) {
		*problems = append(*problems, ValidationError{Source: source, Line: lineNo, Text: pl.text, Severity: SeverityWarning, Message: fmt.Sprintf(format, args...)})
	}
	fail := func(format string, args ...any) {
		*problems = append(*problems, ValidationError{Source: source, Line: lineNo, Text: pl.text, Severity: SeverityError, Message: fmt.Sprintf(format, args...)})
	}
	switch pl.kind {
	case lineSkip:
		return
	case lineNoop:
		warn("line type %q is accepted for compatibility but has no effect", pl.tokens[0])
		return
	case lineGroup:
		checkFieldWhitespace(pl.tokens, []string{"member", "role"}, warn)
		if pl.tokens[1] == "" || pl.tokens[2] == "" {
			warn("empty member or role in 'g' line")
		}
		b.addLink(pl.tokens[1], pl.tokens[2], loc)
	case linePolicy:
		sub, res, act, obj, eft := pl.tokens[1], pl.tokens[2], pl.tokens[3], pl.tokens[4], pl.tokens[5]
		checkFieldWhitespace(pl.tokens, []string{"subject", "resource", "action", "object", "effect"}, warn)
		if sub == "" {
			warn("empty subject: the rule only applies to subjects linked to the empty role")
		}
		e := parseEffect(eft)
		if e == effectNone {
			warn("effect %q is neither %q nor %q: the rule never takes effect", eft, EffectAllow, EffectDeny)
		}
		r := b.addRule(sub, res, act, obj, e, loc)
		for _, f := range []struct {
			name string
			m    *matcher
		}{{"resource", r.res}, {"action", r.act}, {"object", r.obj}} {
			msg := f.m.problem(f.name, b.mode)
			switch {
			case msg == "":
			case f.m.kind == matchNever:
				// Upstream parity: an invalid glob is a rule that never matches. It
				// cannot fail an evaluation, so it stays a warning.
				warn("%s: the rule never matches", msg)
			case src.Builtin:
				warn("%s: requests reaching this rule are denied", msg)
			default:
				fail("%s: fix the pattern; a policy containing it is not loaded", msg)
			}
		}
		if isLiteralToken(res, b.mode) && !isKnownResource(res) {
			warn("resource %q is not a known RBAC resource", res)
		}
		if isLiteralToken(act, b.mode) && !isKnownActionToken(act) {
			warn("action %q is not a known RBAC action", act)
		}
	}
}

// checkFieldWhitespace warns about fields with trailing whitespace: CSV parsing only
// trims leading spaces, so such fields are compared literally, spaces included.
func checkFieldWhitespace(tokens, names []string, warn func(string, ...any)) {
	for i, name := range names {
		f := tokens[i+1]
		if strings.TrimRightFunc(f, unicode.IsSpace) != f {
			warn("%s %q has trailing whitespace, which is part of the value", name, f)
		}
	}
}

// isKnownActionToken reports whether act is a baseline action, "*", or one of the
// application path forms.
func isKnownActionToken(act string) bool {
	if act == ActionAll {
		return true
	}
	for _, a := range actions {
		if act == a {
			return true
		}
	}
	for _, prefix := range []string{ActionAction + "/", ActionUpdate + "/", ActionDelete + "/"} {
		if strings.HasPrefix(act, prefix) {
			return true
		}
	}
	return false
}
