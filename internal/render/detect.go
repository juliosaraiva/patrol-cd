package render

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// KustomizationNames are the file names that mark a Kustomize directory, in lookup
// order (case-sensitive, as upstream).
var KustomizationNames = []string{"kustomization.yaml", "kustomization.yml", "Kustomization"}

// IsKustomization reports whether name is a kustomization file name.
func IsKustomization(name string) bool { return slices.Contains(KustomizationNames, name) }

// IsSourceTypeEnabled reports whether sourceType is enabled in enabled (a nil map or
// a missing key means enabled), as upstream IsManifestGenerationEnabled. Plugin is
// always enabled: no setting can disable it (upstream's controller always sends
// Plugin=true), so an explicit plugin source is never downgraded to Directory.
func IsSourceTypeEnabled(sourceType v1alpha1.ApplicationSourceType, enabled map[string]bool) bool {
	if enabled == nil || sourceType == v1alpha1.ApplicationSourceTypePlugin {
		return true
	}
	v, ok := enabled[string(sourceType)]
	return !ok || v
}

// ExplicitType returns the type fixed by the source's tool block, nil when none is
// set, or the upstream error when more than one is set.
func ExplicitType(source *v1alpha1.ApplicationSource) (*v1alpha1.ApplicationSourceType, error) {
	var types []v1alpha1.ApplicationSourceType
	if source.Kustomize != nil {
		types = append(types, v1alpha1.ApplicationSourceTypeKustomize)
	}
	if source.Helm != nil {
		types = append(types, v1alpha1.ApplicationSourceTypeHelm)
	}
	if source.Directory != nil {
		types = append(types, v1alpha1.ApplicationSourceTypeDirectory)
	}
	if source.Plugin != nil {
		types = append(types, v1alpha1.ApplicationSourceTypePlugin)
	}
	switch len(types) {
	case 0:
		return nil, nil
	case 1:
		return &types[0], nil
	}
	names := make([]string, len(types))
	for i, t := range types {
		names[i] = string(t)
	}
	return nil, fmt.Errorf("multiple application sources defined: %s", strings.Join(names, ","))
}

// sourceType merges the override files into c.src and determines the tool (SRC-001,
// SRC-002, SRC-003).
func (r *Renderer) sourceType(ctx context.Context, c *call) (v1alpha1.ApplicationSourceType, error) {
	if err := mergeSourceParameters(c.src, c.root, c.appPath, c.in.AppName, c.limits.MaxOverrideFileBytes); err != nil {
		return "", fmt.Errorf("error while parsing source parameters: %w", err)
	}
	explicit, err := ExplicitType(c.src)
	if err != nil {
		return "", err
	}
	if explicit != nil {
		if !IsSourceTypeEnabled(*explicit, c.in.EnabledSourceTypes) {
			c.log.Debug("manifest generation disabled for the source type, rendering as Directory", "sourceType", string(*explicit))
			return v1alpha1.ApplicationSourceTypeDirectory, nil
		}
		return *explicit, nil
	}
	// CMP discovery first; any failure (no socket directory, no match, probe
	// errors) falls back to the built-in tools, as upstream.
	if conn, err := r.detectPlugin(ctx, c, "", c.env.Environ()); err == nil {
		conn.close()
		return v1alpha1.ApplicationSourceTypePlugin, nil
	} else if ctxErr := ctx.Err(); ctxErr != nil {
		return "", ctxErr
	}
	t, err := detectBuiltinType(c.appPath, c.in.EnabledSourceTypes)
	if err != nil {
		return "", err
	}
	return t, nil
}

// detectBuiltinType inspects the files directly in appPath: a file whose name ends
// with "Chart.yaml" marks Helm, a kustomization file marks Kustomize; when both exist
// the one later in lexical order wins (upstream walk semantics, so kustomization.yaml
// beats Chart.yaml). Disabled tools are not detected. Otherwise Directory.
func detectBuiltinType(appPath string, enabled map[string]bool) (v1alpha1.ApplicationSourceType, error) {
	entries, err := os.ReadDir(appPath)
	if err != nil {
		return "", err
	}
	t := v1alpha1.ApplicationSourceTypeDirectory
	for _, e := range entries { // os.ReadDir sorts by name
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasSuffix(name, "Chart.yaml") && IsSourceTypeEnabled(v1alpha1.ApplicationSourceTypeHelm, enabled) {
			t = v1alpha1.ApplicationSourceTypeHelm
		}
		if IsKustomization(name) && IsSourceTypeEnabled(v1alpha1.ApplicationSourceTypeKustomize, enabled) {
			t = v1alpha1.ApplicationSourceTypeKustomize
		}
	}
	return t, nil
}

// Discover lists the Helm and Kustomize application directories below appPath
// (relative paths, "." for appPath itself), as upstream discovery.Discover for
// ListApps. When a plugin claims appPath, the result is {".": "Plugin"}. Symlinked
// directories are not followed. The walk stops at ctx cancellation.
func (r *Renderer) Discover(ctx context.Context, root, appPath string, enabled map[string]bool) (map[string]string, error) {
	c := &call{root: filepath.Clean(root), appPath: appPath, in: Input{EnabledSourceTypes: enabled}, limits: r.cfg.Limits.normalized(), log: r.log, env: &buildEnv{}}
	if conn, err := r.detectPlugin(ctx, c, "", nil); err == nil {
		conn.close()
		return map[string]string{".": string(v1alpha1.ApplicationSourceTypePlugin)}, nil
	}
	apps := map[string]string{}
	err := filepath.WalkDir(appPath, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		dir, err := filepath.Rel(appPath, filepath.Dir(p))
		if err != nil {
			return err
		}
		base := d.Name()
		if strings.HasSuffix(base, "Chart.yaml") && IsSourceTypeEnabled(v1alpha1.ApplicationSourceTypeHelm, enabled) {
			apps[dir] = string(v1alpha1.ApplicationSourceTypeHelm)
		}
		if IsKustomization(base) && IsSourceTypeEnabled(v1alpha1.ApplicationSourceTypeKustomize, enabled) {
			apps[dir] = string(v1alpha1.ApplicationSourceTypeKustomize)
		}
		return nil
	})
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		return apps, fmt.Errorf("error discovering applications: %w", err)
	}
	return apps, err
}
