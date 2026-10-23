package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var (
	chartVersionRe    = regexp.MustCompile(`(?m)^version: .*$`)
	chartAppVersionRe = regexp.MustCompile(`(?m)^appVersion: .*$`)
)

// chartVersion reads the chart's version from Chart.yaml.
func chartVersion(chartYAML string) (version, error) {
	m := chartVersionRe.FindAllString(chartYAML, -1)
	if len(m) != 1 {
		return version{}, fmt.Errorf("chart: Chart.yaml: want one top-level version line, found %d", len(m))
	}
	return parseVersion(strings.TrimSpace(strings.TrimPrefix(m[0], "version:")))
}

// setChartVersion sets the chart's version and appVersion to v: the chart and the image are
// released together with one number.
func setChartVersion(chartYAML string, v version) (string, error) {
	if n := len(chartVersionRe.FindAllString(chartYAML, -1)); n != 1 {
		return "", fmt.Errorf("chart: Chart.yaml: want one top-level version line, found %d", n)
	}
	if n := len(chartAppVersionRe.FindAllString(chartYAML, -1)); n != 1 {
		return "", fmt.Errorf("chart: Chart.yaml: want one top-level appVersion line, found %d", n)
	}
	chartYAML = chartVersionRe.ReplaceAllLiteralString(chartYAML, "version: "+v.String())
	return chartAppVersionRe.ReplaceAllLiteralString(chartYAML, `appVersion: "`+v.String()+`"`), nil
}

// setImageRegistry sets image.registry (the Patrol image's registry, top-level key "image") in
// the chart's values.yaml. The edit is textual so that comments and order, which helm-docs and the
// chart README read, stay as they are.
func setImageRegistry(valuesYAML, registry string) (string, error) {
	lines := strings.SplitAfter(valuesYAML, "\n")
	inImage, done := false, false
	for i, l := range lines {
		trimmed := strings.TrimRight(l, "\r\n")
		switch {
		case trimmed == "image:":
			inImage = true
		case inImage && trimmed != "" && !strings.HasPrefix(trimmed, " ") && !strings.HasPrefix(trimmed, "#"):
			inImage = false
		case inImage && strings.HasPrefix(trimmed, "  registry:"):
			if done {
				return "", errors.New("values.yaml: image.registry appears twice")
			}
			lines[i] = `  registry: "` + registry + `"` + l[len(trimmed):]
			done = true
		}
	}
	if !done {
		return "", errors.New("values.yaml: no image.registry key under the top-level image key")
	}
	return strings.Join(lines, ""), nil
}

// imageLineRe matches the Patrol image in a rendered manifest: "image: REPOSITORY:TAG".
func imageLineRe(repository string) *regexp.Regexp {
	return regexp.MustCompile(`(?m)^(\s*(?:- )?image: )"?` + regexp.QuoteMeta(repository) + `:([^\s"@]+)"?[ \t]*$`)
}

// retagManifest replaces every "image: REPOSITORY:TAG" with "image: NEWREF:TAG" and fails when a
// tag is not the release version (stale manifests). It returns the number of replacements.
func retagManifest(manifest, repository, newRef string, v version) (string, int, error) {
	re := imageLineRe(repository)
	var bad []string
	n := 0
	out := re.ReplaceAllStringFunc(manifest, func(line string) string {
		m := re.FindStringSubmatch(line)
		if m[2] != v.String() {
			bad = append(bad, m[2])
		}
		n++
		return m[1] + newRef + ":" + m[2]
	})
	if len(bad) > 0 {
		return "", 0, fmt.Errorf("image %s has tag %s, want %s (regenerate the manifests: make manifests-install)", repository, bad[0], v)
	}
	return out, n, nil
}

// releaseManifests maps the single-file install manifests of deploy/manifests to their release
// asset names (as upstream names them: install.yaml, ha-install.yaml, ...).
func releaseManifests(manifestsDir string) (map[string]string, error) {
	assets := map[string]string{}
	for _, sub := range []string{"", "ha"} {
		matches, err := filepath.Glob(filepath.Join(manifestsDir, sub, "*.yaml"))
		if err != nil {
			return nil, err
		}
		for _, m := range matches {
			base := filepath.Base(m)
			if base == "kustomization.yaml" {
				continue
			}
			if sub != "" {
				base = sub + "-" + base
			}
			assets[m] = base
		}
	}
	if len(assets) == 0 {
		return nil, fmt.Errorf("no install manifests in %s", manifestsDir)
	}
	return assets, nil
}

// stageManifests writes the install manifests, pointed at the published image, into outDir.
func stageManifests(manifestsDir, outDir, repository, newRef string, v version) error {
	assets, err := releaseManifests(manifestsDir)
	if err != nil {
		return err
	}
	total := 0
	for src, name := range assets {
		data, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		out, n, err := retagManifest(string(data), repository, newRef, v)
		if err != nil {
			return fmt.Errorf("%s: %w", src, err)
		}
		total += n
		if err := os.WriteFile(filepath.Join(outDir, name), []byte(out), 0o644); err != nil {
			return err
		}
	}
	if total == 0 {
		return fmt.Errorf("no %s image reference found in %s", repository, manifestsDir)
	}
	return nil
}

// stageChart copies the chart to outDir with image.registry set, ready for `helm package`.
func stageChart(chartDir, outDir, registry string) error {
	err := filepath.WalkDir(chartDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(chartDir, path)
		if err != nil {
			return err
		}
		dst := filepath.Join(outDir, rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s: not a regular file", path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if rel == "values.yaml" {
			s, err := setImageRegistry(string(data), registry)
			if err != nil {
				return err
			}
			data = []byte(s)
		}
		return os.WriteFile(dst, data, 0o644)
	})
	return err
}
