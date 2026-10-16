package hydrator

import (
	"crypto/sha256"
	"encoding/hex"
	"path"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/project"
)

// Key groups Applications hydrated together in one commit (upstream HydrationQueueKey):
// the normalized dry repository and dry targetRevision, and the normalized destination
// repository and branch (hydrateTo.targetBranch, else syncSource.targetBranch).
type Key struct {
	SourceRepoURL        string
	SourceTargetRevision string
	DestinationRepoURL   string
	DestinationBranch    string
}

// KeyOf returns the hydration key of app, and false when it has no source hydrator.
func KeyOf(app *v1alpha1.Application) (Key, bool) {
	if app == nil || app.Spec.SourceHydrator == nil {
		return Key{}, false
	}
	sh := app.Spec.SourceHydrator
	dest := hydrateToSource(sh)
	return Key{
		SourceRepoURL:        normalizeURL(sh.DrySource.RepoURL),
		SourceTargetRevision: sh.DrySource.TargetRevision,
		DestinationRepoURL:   normalizeURL(dest.RepoURL),
		DestinationBranch:    dest.TargetRevision,
	}, true
}

// indexValue is the field-index value of the key (a digest: index values must be short and
// the fields may contain any character).
func (k Key) indexValue() string {
	h := sha256.New()
	for _, f := range []string{k.SourceRepoURL, k.SourceTargetRevision, k.DestinationRepoURL, k.DestinationBranch} {
		h.Write([]byte(f))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// keyIndexField is the cache index of Applications by hydration key.
const keyIndexField = "cd.patrol.example/hydration-key"

// indexByKey is the client.IndexerFunc of keyIndexField.
func indexByKey(obj client.Object) []string {
	app, ok := obj.(*v1alpha1.Application)
	if !ok {
		return nil
	}
	k, ok := KeyOf(app)
	if !ok {
		return nil
	}
	return []string{k.indexValue()}
}

// hydrateToSource is where hydrated manifests are committed: the sync source, on the
// hydrateTo branch when set.
func hydrateToSource(sh *v1alpha1.SourceHydrator) v1alpha1.ApplicationSource {
	src := sh.GetSyncSource()
	if sh.HydrateTo != nil {
		src.TargetRevision = sh.HydrateTo.TargetBranch
	}
	return src
}

// normalizeURL normalizes repository URLs for grouping; an URL that cannot be normalized is
// used as is (upstream NormalizeGitURLAllowInvalid).
func normalizeURL(u string) string {
	if n := project.NormalizeGitURL(u); n != "" {
		return n
	}
	return u
}

// isRootPath reports whether p designates the repository root (upstream IsRootPath).
func isRootPath(p string) bool {
	c := path.Clean(p)
	return c == "" || c == "." || c == "/"
}

// isOutsideRepository reports whether p escapes the repository (absolute or "..").
func isOutsideRepository(p string) bool {
	c := path.Clean(p)
	return path.IsAbs(c) || c == ".." || strings.HasPrefix(c, "../")
}

// refreshPaths resolves the manifest-generate-paths annotation against the dry source path
// (APP-133): entries are separated by ';' and trimmed; an entry starting with '/' is
// relative to the repository root, others to the source path.
func refreshPaths(annotation, sourcePath string) []string {
	var out []string
	for item := range strings.SplitSeq(annotation, ";") {
		item = strings.TrimSpace(item)
		switch {
		case item == "":
		case strings.HasPrefix(item, "/"):
			out = append(out, strings.TrimPrefix(item, "/"))
		default:
			out = append(out, path.Clean(path.Join(sourcePath, item)))
		}
	}
	return out
}
