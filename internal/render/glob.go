package render

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/gobwas/glob"
	"github.com/gobwas/glob/syntax"
)

// Directory include/exclude patterns are gobwas/glob patterns compiled without
// separators (upstream util/glob.Match), so '*' also matches '/'. Patterns come from
// Application specs; to keep matching linear-time, patterns with alternations
// ('{a,b}'), whose backtracking walk is exponential in gobwas/glob, are matched with an
// equivalent RE2 expression (the translation follows internal/rbac). Patterns without
// alternations stay on gobwas/glob, which matches them in O(len(pattern)*len(path)).

// errGlobUntranslatable reports an alternation pattern without an exact RE2
// equivalent; such a pattern matches nothing (as an invalid pattern does upstream).
var errGlobUntranslatable = errors.New("glob pattern with alternations cannot be evaluated safely")

// dirGlob is a compiled include/exclude pattern.
type dirGlob struct {
	g  *glob.Pattern
	re *regexp.Regexp
}

func (d *dirGlob) match(s string) bool {
	if d.re != nil {
		return d.re.MatchString(s)
	}
	return d.g.Match(s)
}

// compileDirGlob compiles pattern; an error means the pattern matches nothing.
func compileDirGlob(pattern string) (*dirGlob, error) {
	g, err := glob.Compile(pattern)
	if err != nil {
		return nil, err
	}
	if !globHasAlternation(pattern) {
		return &dirGlob{g: g}, nil
	}
	expr, err := globToRegexp(pattern)
	if err != nil {
		return nil, err
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errGlobUntranslatable, err)
	}
	return &dirGlob{re: re}, nil
}

// globHasAlternation reports whether pattern has a '{...}' group with a ','.
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

// globToRegexp translates a pattern that glob.Compile accepted into an anchored RE2
// expression with the same matches, using gobwas/glob's own lexer:
//
//	'*', '**'         -> .*  ((?s): newlines match too)
//	'?'               -> .
//	'[abc]', '[!a-z]' -> a class of \x{...} runes, negated with ^
//	'{a,b,}'          -> (?:a|b|)
//	text              -> regexp.QuoteMeta(text)
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
				return "", fmt.Errorf("%w: unclosed '{'", errGlobUntranslatable)
			}
			b.WriteString(`)\z`)
			return b.String(), nil
		case syntax.Error:
			return "", fmt.Errorf("%w: %s", errGlobUntranslatable, tok.Data)
		case syntax.Text:
			if strings.ContainsRune(tok.Data, utf8.RuneError) {
				return "", fmt.Errorf("%w: literal U+FFFD", errGlobUntranslatable)
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
				return "", fmt.Errorf("%w: ',' outside '{...}'", errGlobUntranslatable)
			}
			b.WriteString(`|`)
		case syntax.TermsClose:
			if depth == 0 {
				return "", fmt.Errorf("%w: unexpected '}'", errGlobUntranslatable)
			}
			depth--
			b.WriteString(`)`)
		default:
			return "", fmt.Errorf("%w: unexpected token %v", errGlobUntranslatable, tok)
		}
	}
}

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
				return "", fmt.Errorf("%w: malformed character class", errGlobUntranslatable)
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
			return "", fmt.Errorf("%w: unexpected token %v in character class", errGlobUntranslatable, tok)
		}
	}
}
