package settings

import (
	"fmt"
	"text/template"

	"github.com/expr-lang/expr"
	"sigs.k8s.io/yaml"
)

// DeepLink is one entry of application.links, resource.links or project.links (API-045).
type DeepLink struct {
	// URL is a Go text/template rendered with sprig functions (minus env, expandenv and
	// getHostByName).
	URL   string `json:"url"`
	Title string `json:"title"`
	// Description, IconClass and Condition are optional.
	Description *string `json:"description,omitempty"`
	IconClass   *string `json:"icon.class,omitempty"`
	// Condition is an expr-lang boolean expression ("if" in YAML).
	Condition *string `json:"if,omitempty"`
}

func (l DeepLink) clone() DeepLink {
	cp := func(s *string) *string {
		if s == nil {
			return nil
		}
		v := *s
		return &v
	}
	l.Description, l.IconClass, l.Condition = cp(l.Description), cp(l.IconClass), cp(l.Condition)
	return l
}

// parseDeepLinks decodes a deep link list. Invalid YAML fails the key; links whose URL
// template or condition does not compile are kept (upstream evaluates and skips them per
// request) and reported as warnings.
func (p *parser) parseDeepLinks(key, v string) ([]DeepLink, error) {
	links := make([]DeepLink, 0)
	if err := yaml.Unmarshal([]byte(v), &links); err != nil {
		return nil, fmt.Errorf("error unmarshalling deep links %w", err)
	}
	for i, l := range links {
		if l.URL == "" {
			p.warn(SourceConfigMap, key, "link %d (%q) has no url", i, l.Title)
		}
		if l.Title == "" {
			p.warn(SourceConfigMap, key, "link %d has no title", i)
		}
		if _, err := template.New("link").Funcs(templateFuncs()).Parse(l.URL); err != nil {
			p.warn(SourceConfigMap, key, "link %d (%q): url template does not parse and the link will be skipped: %v", i, l.Title, err)
		}
		if l.Condition != nil {
			if _, err := expr.Compile(*l.Condition, expr.AllowUndefinedVariables()); err != nil {
				p.warn(SourceConfigMap, key, "link %d (%q): condition does not compile and the link will be skipped: %v", i, l.Title, err)
			}
		}
	}
	return links, nil
}
