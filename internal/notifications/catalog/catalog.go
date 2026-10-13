// Package catalog embeds the default notification triggers and templates (derived from the
// Argo CD v3.5.5 notifications catalog, Apache-2.0; see README.md).
package catalog

import (
	"embed"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
)

//go:embed triggers/*.yaml templates/*.yaml
var files embed.FS

// Data returns the catalog as notifications ConfigMap entries: "trigger.<name>" and
// "template.<name>" mapped to their YAML definitions.
func Data() (map[string]string, error) {
	out := map[string]string{}
	for dir, prefix := range map[string]string{"triggers": "trigger.", "templates": "template."} {
		entries, err := fs.ReadDir(files, dir)
		if err != nil {
			return nil, fmt.Errorf("catalog: reading %s: %w", dir, err)
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
				continue
			}
			b, err := files.ReadFile(path.Join(dir, e.Name()))
			if err != nil {
				return nil, fmt.Errorf("catalog: reading %s: %w", e.Name(), err)
			}
			out[prefix+strings.TrimSuffix(e.Name(), ".yaml")] = string(b)
		}
	}
	return out, nil
}

// TriggerNames returns the sorted names of the catalog triggers.
func TriggerNames() []string { return names("triggers") }

// TemplateNames returns the sorted names of the catalog templates.
func TemplateNames() []string { return names("templates") }

func names(dir string) []string {
	entries, err := fs.ReadDir(files, dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".yaml") {
			out = append(out, strings.TrimSuffix(e.Name(), ".yaml"))
		}
	}
	sort.Strings(out)
	return out
}
