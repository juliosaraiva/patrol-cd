package template

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"sigs.k8s.io/yaml"
)

const maxDNSNameLength = 253

var invalidDNSNameChars = regexp.MustCompile("[^-a-z0-9.]")

// SanitizeName normalizes a name into a DNS-1123 subdomain-like value: lower case, every
// character outside [-a-z0-9.] replaced by '-', truncated to 253 bytes, and leading/trailing
// '-' and '.' trimmed. It is the "normalize" template function and the *Normalized parameters
// (nameNormalized, basenameNormalized, branchNormalized, ...).
func SanitizeName(name string) string {
	name = strings.ToLower(name)
	name = invalidDNSNameChars.ReplaceAllString(name, "-")
	if len(name) > maxDNSNameLength {
		name = name[:maxDNSNameLength]
	}
	return strings.Trim(name, "-.")
}

// Slugify is the "slugify" template function: slugify [maxSize=50] [smartTruncate=true] name.
// The first argument (when there are two or three) is the maximum length, the second (when there
// are three) enables smart truncation (cut at a word boundary), and the last is the input. The
// input is normalized with SanitizeName before slugging.
func Slugify(args ...any) (string, error) {
	maxSize := 50
	smart := true
	name := ""
	for i, arg := range args {
		switch {
		case i == len(args)-1:
			s, ok := arg.(string)
			if !ok {
				return "", fmt.Errorf("slugify: the last argument must be a string, got %T", arg)
			}
			name = s
		case i == 0:
			n, ok := arg.(int)
			if !ok {
				return "", fmt.Errorf("slugify: the first argument must be an int, got %T", arg)
			}
			maxSize = n
		case i == 1:
			b, ok := arg.(bool)
			if !ok {
				return "", fmt.Errorf("slugify: the second argument must be a bool, got %T", arg)
			}
			smart = b
		default:
			return "", fmt.Errorf("slugify: too many arguments (%d)", len(args))
		}
	}
	return Slug(SanitizeName(name), SlugOptions{MaxLength: maxSize, SmartTruncate: smart}), nil
}

// SlugOptions configures Slug.
type SlugOptions struct {
	// MaxLength truncates the slug; 0 or less means no limit.
	MaxLength int
	// SmartTruncate cuts at the last '-' before MaxLength instead of at MaxLength.
	SmartTruncate bool
	// Substitutions are applied (in sorted key order) before slugging, e.g. {"_": "-"} for
	// pull request branch slugs.
	Substitutions map[string]string
}

var (
	slugNonAuthorized = regexp.MustCompile("[^a-zA-Z0-9-_]")
	slugMultipleDash  = regexp.MustCompile("-+")
)

// slugRuneSub mirrors the English substitution table of github.com/gosimple/slug (with its
// default quote and dash substitutions).
var slugRuneSub = map[rune]string{
	'"':  "",
	'\'': "",
	'’':  "",
	'‒':  "-",
	'–':  "-",
	'—':  "-",
	'―':  "-",
	'&':  "and",
	'@':  "at",
}

// Slug produces a URL-safe slug with the semantics of github.com/gosimple/slug (Make): trim,
// custom substitutions, English substitutions, transliteration of common Latin characters,
// lower case, characters outside [a-zA-Z0-9-_] to '-', repeated '-' collapsed, '-' and '_'
// trimmed, then truncation. Non-Latin characters that have no ASCII transliteration here become
// '-' (gosimple/slug transliterates them with unidecode; documented difference).
func Slug(s string, o SlugOptions) string {
	slug := strings.TrimSpace(s)
	if len(o.Substitutions) > 0 {
		for _, k := range sortedKeys(o.Substitutions) {
			slug = strings.ReplaceAll(slug, k, o.Substitutions[k])
		}
	}
	var b strings.Builder
	for _, r := range slug {
		if sub, ok := slugRuneSub[r]; ok {
			b.WriteString(sub)
			continue
		}
		if r < utf8.RuneSelf {
			b.WriteRune(r)
			continue
		}
		if t, ok := transliterate[r]; ok {
			b.WriteString(t)
			continue
		}
		b.WriteRune(r)
	}
	slug = strings.ToLower(b.String())
	// As gosimple/slug: without smart truncation the (lower-cased) input is cut before the
	// character clean-up, so a maximum length of 0 yields an empty slug.
	if !o.SmartTruncate && o.MaxLength >= 0 && len(slug) >= o.MaxLength {
		slug = slug[:o.MaxLength]
	}
	slug = slugNonAuthorized.ReplaceAllString(slug, "-")
	slug = slugMultipleDash.ReplaceAllString(slug, "-")
	slug = strings.Trim(slug, "-_")
	if o.MaxLength > 0 && o.SmartTruncate {
		slug = smartTruncate(slug, o.MaxLength)
	}
	return slug
}

func smartTruncate(text string, maxLength int) string {
	if len(text) <= maxLength {
		return text
	}
	for i := maxLength; i >= 0; i-- {
		if text[i] == '-' {
			return text[:i]
		}
	}
	return text[:maxLength]
}

// transliterate covers Latin-1 Supplement and Latin Extended-A letters, which is what branch and
// repository names realistically contain.
var transliterate = map[rune]string{
	'À': "A", 'Á': "A", 'Â': "A", 'Ã': "A", 'Ä': "A", 'Å': "A", 'Æ': "AE", 'Ç': "C",
	'È': "E", 'É': "E", 'Ê': "E", 'Ë': "E", 'Ì': "I", 'Í': "I", 'Î': "I", 'Ï': "I",
	'Ð': "D", 'Ñ': "N", 'Ò': "O", 'Ó': "O", 'Ô': "O", 'Õ': "O", 'Ö': "O", 'Ø': "O",
	'Ù': "U", 'Ú': "U", 'Û': "U", 'Ü': "U", 'Ý': "Y", 'Þ': "Th", 'ß': "ss",
	'à': "a", 'á': "a", 'â': "a", 'ã': "a", 'ä': "a", 'å': "a", 'æ': "ae", 'ç': "c",
	'è': "e", 'é': "e", 'ê': "e", 'ë': "e", 'ì': "i", 'í': "i", 'î': "i", 'ï': "i",
	'ð': "d", 'ñ': "n", 'ò': "o", 'ó': "o", 'ô': "o", 'õ': "o", 'ö': "o", 'ø': "o",
	'ù': "u", 'ú': "u", 'û': "u", 'ü': "u", 'ý': "y", 'þ': "th", 'ÿ': "y",
	'Ā': "A", 'ā': "a", 'Ă': "A", 'ă': "a", 'Ą': "A", 'ą': "a", 'Ć': "C", 'ć': "c",
	'Č': "C", 'č': "c", 'Ď': "D", 'ď': "d", 'Đ': "D", 'đ': "d", 'Ē': "E", 'ē': "e",
	'Ė': "E", 'ė': "e", 'Ę': "E", 'ę': "e", 'Ě': "E", 'ě': "e", 'Ğ': "G", 'ğ': "g",
	'Ī': "I", 'ī': "i", 'Į': "I", 'į': "i", 'İ': "I", 'ı': "i", 'Ł': "L", 'ł': "l",
	'Ń': "N", 'ń': "n", 'Ň': "N", 'ň': "n", 'Ō': "O", 'ō': "o", 'Ő': "O", 'ő': "o",
	'Œ': "OE", 'œ': "oe", 'Ř': "R", 'ř': "r", 'Ś': "S", 'ś': "s", 'Ş': "S", 'ş': "s",
	'Š': "S", 'š': "s", 'Ţ': "T", 'ţ': "t", 'Ť': "T", 'ť': "t", 'Ū': "U", 'ū': "u",
	'Ů': "U", 'ů': "u", 'Ű': "U", 'ű': "u", 'Ų': "U", 'ų': "u", 'Ÿ': "Y", 'Ź': "Z",
	'ź': "z", 'Ż': "Z", 'ż': "z", 'Ž': "Z", 'ž': "z",
}

// toYAML marshals v with sigs.k8s.io/yaml and strips the trailing newline. Unlike Helm's
// toYaml, errors fail the template execution.
func toYAML(v any) (string, error) {
	data, err := yaml.Marshal(v)
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(string(data), "\n"), nil
}

// fromYAML parses a YAML object.
func fromYAML(str string) (map[string]any, error) {
	m := map[string]any{}
	if err := yaml.Unmarshal([]byte(str), &m); err != nil {
		return nil, err
	}
	return m, nil
}

// fromYAMLArray parses a YAML list.
func fromYAMLArray(str string) ([]any, error) {
	a := []any{}
	if err := yaml.Unmarshal([]byte(str), &a); err != nil {
		return nil, err
	}
	return a, nil
}
