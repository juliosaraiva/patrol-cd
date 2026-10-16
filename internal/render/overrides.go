package render

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	jsonpatch "github.com/evanphx/json-patch/v5"
	"sigs.k8s.io/yaml"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/sandbox"
)

// Parameter override file names (SRC-092, SRC-093). The upstream names are honored so
// repositories maintained for Argo CD (for example by Argo CD Image Updater) keep
// working; the Patrol names win over them at the same specificity.
const (
	// LegacySourceFile is upstream's generic override file.
	LegacySourceFile = ".argocd-source.yaml"
	// SourceFile is Patrol's generic override file.
	SourceFile = ".patrol-source.yaml"
	// legacyAppSourceFile and appSourceFile are the application-specific forms
	// (formatted with the instance name).
	legacyAppSourceFile = ".argocd-source-%s.yaml"
	appSourceFile       = ".patrol-source-%s.yaml"
)

// OverrideFiles returns the override file names merged for appName, in merge order
// (later files win): generic legacy, generic Patrol, app-specific legacy, app-specific
// Patrol. appName is the instance name; app-specific files are skipped when it is
// empty.
func OverrideFiles(appName string) []string {
	files := []string{LegacySourceFile, SourceFile}
	if appName != "" {
		files = append(files, fmt.Sprintf(legacyAppSourceFile, appName), fmt.Sprintf(appSourceFile, appName))
	}
	return files
}

// mergeSourceParameters merges the override files found in appPath into source with
// RFC 7386 merge-patch semantics, then restores repoURL, path, chart and
// targetRevision, which overrides cannot change. Override files must be regular files
// that resolve inside root; directories with those names are ignored.
func mergeSourceParameters(source *v1alpha1.ApplicationSource, root, appPath, appName string, maxBytes int64) error {
	merged := source.DeepCopy()
	appRel, err := relToRoot(root, appPath)
	if err != nil {
		return err
	}
	for _, name := range OverrideFiles(appName) {
		if filepath.Base(name) != name || name == "." || name == ".." {
			// An instance name containing a separator cannot name a file in appPath.
			continue
		}
		p, err := sandbox.SecureJoin(root, filepath.Join(appRel, name))
		if err != nil {
			if errors.Is(err, sandbox.ErrPathEscape) {
				return fmt.Errorf("%s: override file resolves outside the repository", name)
			}
			return fmt.Errorf("%s: %w", name, err)
		}
		info, err := os.Stat(p)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			continue
		case err != nil:
			return fmt.Errorf("%s: %w", name, err)
		case info.IsDir():
			continue
		case !info.Mode().IsRegular():
			return fmt.Errorf("%s: not a regular file", name)
		}
		patch, err := readFileBounded(p, maxBytes)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if err := applyOverride(merged, patch); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	merged.Chart = source.Chart
	merged.Path = source.Path
	merged.RepoURL = source.RepoURL
	merged.TargetRevision = source.TargetRevision
	*source = *merged
	return nil
}

func applyOverride(src *v1alpha1.ApplicationSource, patchYAML []byte) error {
	patch, err := yaml.YAMLToJSON(patchYAML)
	if err != nil {
		return err
	}
	if string(patch) == "null" {
		// An empty file is an empty patch.
		return nil
	}
	data, err := json.Marshal(src)
	if err != nil {
		return err
	}
	data, err = jsonpatch.MergePatch(data, patch)
	if err != nil {
		return err
	}
	var out v1alpha1.ApplicationSource
	if err := json.Unmarshal(data, &out); err != nil {
		return err
	}
	*src = out
	return nil
}

// readFileBounded reads a regular file of at most limit bytes (limit <= 0: no cap).
func readFileBounded(p string, limit int64) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	if limit <= 0 {
		return io.ReadAll(f)
	}
	if info.Size() > limit {
		return nil, fmt.Errorf("file exceeds %d bytes", limit)
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("file exceeds %d bytes", limit)
	}
	return data, nil
}
