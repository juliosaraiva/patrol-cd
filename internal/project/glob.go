package project

import (
	"log/slog"
	"net/url"
	"regexp"
	"strings"

	"github.com/gobwas/glob"
	lru "github.com/hashicorp/golang-lru/v2"
)

// compiledCacheSize bounds the number of compiled patterns kept in memory. Patterns come from
// AppProjects (written by project editors) and Applications, so the cache must be bounded;
// the size matches upstream's DefaultGlobCacheSize.
const compiledCacheSize = 10000

type globKey struct {
	pattern    string
	separators string
}

type compiledGlob struct {
	g   *glob.Pattern
	err error
}

type compiledRegexp struct {
	re  *regexp.Regexp
	err error
}

var (
	globCache   = newCache[globKey, compiledGlob](compiledCacheSize)
	regexpCache = newCache[string, compiledRegexp](compiledCacheSize)
)

// cache is a bounded, concurrency-safe LRU. A nil *lru.Cache (impossible with the constant
// size above) degrades to no caching instead of failing.
type cache[K comparable, V any] struct{ c *lru.Cache[K, V] }

func newCache[K comparable, V any](size int) cache[K, V] {
	c, err := lru.New[K, V](size)
	if err != nil {
		return cache[K, V]{}
	}
	return cache[K, V]{c: c}
}

func (c cache[K, V]) Get(k K) (V, bool) {
	if c.c == nil {
		var zero V
		return zero, false
	}
	return c.c.Get(k)
}

func (c cache[K, V]) Add(k K, v V) {
	if c.c != nil {
		c.c.Add(k, v)
	}
}

// compileGlob compiles (and caches, including failures) a gobwas glob. A pattern that fails
// to compile is logged once per cache lifetime, like upstream's warning.
func compileGlob(pattern string, separators ...rune) (*glob.Pattern, error) {
	key := globKey{pattern: pattern, separators: string(separators)}
	if c, ok := globCache.Get(key); ok {
		return c.g, c.err
	}
	g, err := glob.Compile(pattern, separators...)
	globCache.Add(key, compiledGlob{g: g, err: err})
	if err != nil {
		slog.Default().Warn("project: invalid glob pattern never matches", "pattern", redactPattern(pattern), "error", err)
	}
	return g, err
}

// globMatchErr matches text against a gobwas glob pattern.
func globMatchErr(pattern, text string, separators ...rune) (bool, error) {
	g, err := compileGlob(pattern, separators...)
	if err != nil {
		return false, err
	}
	return g.Match(text), nil
}

// globMatch is upstream glob.Match: a pattern that fails to compile never matches.
func globMatch(pattern, text string, separators ...rune) bool {
	ok, err := globMatchErr(pattern, text, separators...)
	return err == nil && ok
}

// isDenyPattern reports whether pattern is a negated ("!"-prefixed) entry.
func isDenyPattern(pattern string) bool {
	return strings.HasPrefix(pattern, "!")
}

// matchPattern is upstream AppProject globMatch: with allowNegation a "!p" pattern matches
// when text does NOT match p (so an invalid negated pattern matches everything, as
// upstream); the literal "*" matches everything, including the empty string.
func matchPattern(pattern, text string, allowNegation bool, separators ...rune) bool {
	if allowNegation && isDenyPattern(pattern) {
		return !globMatch(pattern[1:], text, separators...)
	}
	if pattern == "*" {
		return true
	}
	return globMatch(pattern, text, separators...)
}

// matchRegexp matches text against an unanchored RE2 regular expression. An expression that
// fails to compile never matches (logged once). Upstream uses .NET-style regexp2, so
// lookaround assertions are not supported here (see the component doc).
func matchRegexp(expr, text string) bool {
	c, ok := regexpCache.Get(expr)
	if !ok {
		re, err := regexp.Compile(expr)
		c = compiledRegexp{re: re, err: err}
		regexpCache.Add(expr, c)
		if err != nil {
			slog.Default().Warn("project: invalid regular expression never matches", "expression", expr, "error", err)
		}
	}
	return c.err == nil && c.re.MatchString(text)
}

// matchStringInList is upstream glob.MatchStringInList in REGEXP mode: an entry wrapped in
// "/" is tried as a regular expression first, then every entry is tried as a glob.
func matchStringInList(list []string, item string) bool {
	for _, entry := range list {
		if len(entry) >= 2 && strings.HasPrefix(entry, "/") && strings.HasSuffix(entry, "/") && matchRegexp(entry[1:len(entry)-1], item) {
			return true
		}
		if globMatch(entry, item) {
			return true
		}
	}
	return false
}

// redactURL hides the password of a URL with embedded credentials. Other strings are
// returned unchanged. Repository URLs are user input and may carry tokens; they appear in
// condition messages and logs.
func redactURL(s string) string {
	if !strings.Contains(s, "@") {
		return s
	}
	u, err := url.Parse(s)
	if err != nil || u.User == nil {
		return s
	}
	if _, has := u.User.Password(); !has {
		return s
	}
	return u.Redacted()
}

func redactPattern(p string) string {
	if isDenyPattern(p) {
		return "!" + redactURL(p[1:])
	}
	return redactURL(p)
}
