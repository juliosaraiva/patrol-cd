package render

import (
	"errors"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/juliosaraiva/patrol-cd/internal/sandbox"
)

// applicationRootPath returns the directory streamed to a plugin generate call when
// manifest-generate-paths streaming is enabled (SRC-087): the deepest common ancestor
// of the application path and every annotation path (";"-separated; relative to the
// application path, or to the repository root when they start with "/"). ".."
// components are clamped at the repository root and symlinks are resolved inside it;
// entries that cannot be resolved are skipped. Without usable annotation paths the
// repository root is returned.
func applicationRootPath(root, appPath, annotation string, log *slog.Logger) string {
	appRel, err := relToRoot(root, appPath)
	if err != nil {
		return root
	}
	var paths []string
	for _, ap := range strings.Split(annotation, ";") {
		if ap == "" {
			continue
		}
		var unsafe string
		if filepath.IsAbs(ap) {
			unsafe = ap
		} else {
			unsafe = filepath.Join(string(filepath.Separator), appRel, ap)
		}
		// Clamp ".." at the root, as filepath-securejoin does upstream.
		clamped := strings.TrimPrefix(filepath.Clean(string(filepath.Separator)+unsafe), string(filepath.Separator))
		if clamped == "" {
			clamped = "."
		}
		resolved, err := sandbox.SecureJoin(root, clamped)
		if err != nil {
			if !errors.Is(err, sandbox.ErrPathEscape) {
				log.Debug("skipping manifest-generate-paths entry", "path", ap, "error", err)
			} else {
				log.Debug("skipping manifest-generate-paths entry that resolves outside the repository", "path", ap)
			}
			continue
		}
		rel, err := relToRoot(root, resolved)
		if err != nil {
			continue
		}
		paths = append(paths, rel)
	}
	if len(paths) == 0 {
		return root
	}
	common := splitRel(appRel)
	for _, p := range paths {
		parts := splitRel(p)
		n := min(len(common), len(parts))
		i := 0
		for i < n && common[i] == parts[i] {
			i++
		}
		common = common[:i]
	}
	if len(common) == 0 {
		return root
	}
	return filepath.Join(append([]string{root}, common...)...)
}

func splitRel(rel string) []string {
	if rel == "." || rel == "" {
		return nil
	}
	return strings.Split(rel, string(filepath.Separator))
}
