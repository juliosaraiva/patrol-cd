package rbac

import (
	"errors"
	"fmt"
	"regexp"
	resyntax "regexp/syntax"
	"strings"
	"unicode/utf8"

	"github.com/gobwas/glob/syntax"
)

// Glob patterns with alternations ('{a,b}') are not matched with gobwas/glob. Its
// backtracking walk takes time exponential in the number of alternation groups whose
// branches contain '*' (for example '{*a,*aa}{*a,*aa}...b'). Such patterns are
// translated into an equivalent RE2 expression instead. Go's regexp matches in
// O(len(pattern) * len(value)).
//
// Patterns without alternations stay on gobwas/glob. Without separators, a '*' there
// drops every pending restart point when it stores its own, so at most one restart
// point is live and matching is also O(len(pattern) * len(value)).

var (
	// errGlobUntranslatable reports an alternation pattern that has no exact RE2
	// equivalent. Rules containing it are evaluated as errors (denied), never with the
	// exponential matcher.
	errGlobUntranslatable = errors.New("glob pattern with alternations cannot be evaluated safely")
	// errGlobSyntax reports a token stream that gobwas/glob would reject. Callers only
	// translate patterns that glob.Compile accepted, so it signals a bug.
	errGlobSyntax = errors.New("unexpected glob syntax")
)

// globHasAlternation reports whether pattern contains an alternation group with at
// least two branches: a ',' inside '{...}'. A group without commas is a plain
// sequence, which gobwas/glob simplifies away.
func globHasAlternation(pattern string) bool {
	if !strings.Contains(pattern, "{") || !strings.Contains(pattern, ",") {
		return false
	}
	lex := syntax.NewLexer(pattern)
	for {
		switch lex.Next().Type {
		case syntax.TermSeparator:
			return true
		case syntax.EOF, syntax.Error:
			return false
		}
	}
}

// compileGlobRegexp compiles the RE2 equivalent of a glob pattern that glob.Compile
// accepted. Errors wrap errGlobUntranslatable or errGlobSyntax. RE2 rejects some
// translations, for example nesting deeper than its limit of 1000 levels; the error then
// names only the reason, not the (large) generated expression.
func compileGlobRegexp(pattern string) (*regexp.Regexp, error) {
	expr, err := globToRegexp(pattern)
	if err != nil {
		return nil, err
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		var se *resyntax.Error
		if errors.As(err, &se) {
			return nil, fmt.Errorf("%w: %s", errGlobUntranslatable, se.Code)
		}
		return nil, fmt.Errorf("%w: %w", errGlobUntranslatable, err)
	}
	return re, nil
}

// globToRegexp translates a gobwas/glob pattern, compiled without separators (as
// Casbin's globMatch does), into an anchored RE2 expression with the same matches. It
// reads the pattern with gobwas/glob's own lexer, so escapes, character classes and
// group boundaries are tokenized exactly as the glob library does:
//
//	'*', '**'         -> .*  (in (?s) mode, so newlines match too)
//	'?'               -> .   (one rune; an invalid UTF-8 byte counts as one rune in both)
//	'[abc]', '[!a-z]' -> a class of \x{...} runes, negated with ^
//	'{a,b,}'          -> (?:a|b|)
//	text              -> regexp.QuoteMeta(text)
//
// It refuses literal text containing U+FFFD. Go's regexp decodes an invalid UTF-8
// byte in the value as U+FFFD, so the literal would also match invalid bytes. The glob
// literal compares bytes and does not.
func globToRegexp(pattern string) (string, error) {
	var b strings.Builder
	b.WriteString(`(?s)\A(?:`)
	lex := syntax.NewLexer(pattern)
	depth := 0
	for {
		tok := lex.Next()
		switch tok.Type {
		case syntax.EOF:
			if depth != 0 {
				return "", fmt.Errorf("%w: unclosed '{'", errGlobSyntax)
			}
			b.WriteString(`)\z`)
			return b.String(), nil
		case syntax.Error:
			return "", fmt.Errorf("%w: %s", errGlobSyntax, tok.Data)
		case syntax.Text:
			if strings.ContainsRune(tok.Data, utf8.RuneError) {
				return "", fmt.Errorf("%w: literal U+FFFD has no exact regular-expression equivalent", errGlobUntranslatable)
			}
			b.WriteString(regexp.QuoteMeta(tok.Data))
		case syntax.Any, syntax.Super:
			b.WriteString(`.*`)
		case syntax.Single:
			b.WriteString(`.`)
		case syntax.RangeOpen:
			class, err := globClassToRegexp(lex)
			if err != nil {
				return "", err
			}
			b.WriteString(class)
		case syntax.TermsOpen:
			depth++
			b.WriteString(`(?:`)
		case syntax.TermSeparator:
			if depth == 0 {
				return "", fmt.Errorf("%w: ',' outside '{...}'", errGlobSyntax)
			}
			b.WriteString(`|`)
		case syntax.TermsClose:
			if depth == 0 {
				return "", fmt.Errorf("%w: unexpected '}'", errGlobSyntax)
			}
			depth--
			b.WriteString(`)`)
		default:
			return "", fmt.Errorf("%w: unexpected token %v", errGlobSyntax, tok)
		}
	}
}

// globClassToRegexp translates a character class, called after its '[' token. It
// follows the tokens gobwas/glob's parser accepts: an optional negation, then either a
// set of runes or a lo-hi range, then ']'.
func globClassToRegexp(lex *syntax.Lexer) (string, error) {
	var (
		not    bool
		lo, hi rune = -1, -1
		set    string
		hasSet bool
	)
	for {
		tok := lex.Next()
		switch tok.Type {
		case syntax.Not:
			not = true
		case syntax.RangeLo:
			lo, _ = utf8.DecodeRuneInString(tok.Data)
		case syntax.RangeBetween:
		case syntax.RangeHi:
			hi, _ = utf8.DecodeRuneInString(tok.Data)
		case syntax.Text:
			set, hasSet = tok.Data, true
		case syntax.RangeClose:
			isRange := lo >= 0 && hi >= lo
			if isRange == hasSet {
				return "", fmt.Errorf("%w: malformed character class", errGlobSyntax)
			}
			var b strings.Builder
			b.WriteByte('[')
			if not {
				b.WriteByte('^')
			}
			if isRange {
				fmt.Fprintf(&b, `\x{%x}-\x{%x}`, lo, hi)
			} else {
				for _, r := range set {
					fmt.Fprintf(&b, `\x{%x}`, r)
				}
			}
			b.WriteByte(']')
			return b.String(), nil
		default:
			return "", fmt.Errorf("%w: unexpected token %v in character class", errGlobSyntax, tok)
		}
	}
}
