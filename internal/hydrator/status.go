package hydrator

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// maxPatchAttempts bounds status writes retried after optimistic-concurrency conflicts.
const maxPatchAttempts = 5

// errAppGone reports that the Application was deleted while being processed.
var errAppGone = fmt.Errorf("application no longer exists")

// patchSourceHydrator applies mutate to app.Status.SourceHydrator and writes the result with
// a JSON merge patch of the status subresource carrying app's resourceVersion (optimistic
// concurrency). On conflict the Application is re-read through APIReader and mutate is
// applied again, so mutate must be computed from its argument and fixed values only. On
// success app holds the stored object. Nothing is written when mutate changes nothing.
func (h *Hydrator) patchSourceHydrator(ctx context.Context, app *v1alpha1.Application, mutate func(*v1alpha1.Application)) error {
	for attempt := 1; ; attempt++ {
		orig := app.DeepCopy()
		mutate(app)
		if apiequality.Semantic.DeepEqual(orig.Status.SourceHydrator, app.Status.SourceHydrator) {
			return nil
		}
		err := h.d.Client.Status().Patch(ctx, app, client.MergeFromWithOptions(orig, client.MergeFromWithOptimisticLock{}))
		switch {
		case err == nil:
			return nil
		case apierrors.IsNotFound(err):
			return errAppGone
		case !apierrors.IsConflict(err):
			*app = *orig
			return fmt.Errorf("failed to persist hydration status of %s: %w", app.QualifiedName(), err)
		case attempt >= maxPatchAttempts:
			*app = *orig
			return fmt.Errorf("failed to persist hydration status of %s after %d attempts: %w", app.QualifiedName(), attempt, err)
		}
		fresh := &v1alpha1.Application{}
		if err := h.d.APIReader.Get(ctx, client.ObjectKeyFromObject(orig), fresh); err != nil {
			if apierrors.IsNotFound(err) {
				return errAppGone
			}
			return fmt.Errorf("failed to re-read application %s: %w", orig.QualifiedName(), err)
		}
		*app = *fresh
	}
}

// consumeHydrateRequest removes the hydrate annotation when it still holds value (JSON patch
// with a test operation, so a request made after the evaluation is not lost). A changed or
// already removed annotation, or a deleted Application, is not an error.
func (h *Hydrator) consumeHydrateRequest(ctx context.Context, app *v1alpha1.Application, value string) error {
	p := "/metadata/annotations/" + jsonPointerEscape(v1alpha1.AnnotationKeyHydrate)
	ops := []map[string]any{
		{"op": "test", "path": p, "value": value},
		{"op": "remove", "path": p},
	}
	data, err := json.Marshal(ops)
	if err != nil {
		return fmt.Errorf("failed to encode annotation patch: %w", err)
	}
	err = h.d.Client.Patch(ctx, app, client.RawPatch(types.JSONPatchType, data))
	switch {
	case err == nil, apierrors.IsNotFound(err), apierrors.IsInvalid(err), apierrors.IsConflict(err):
		return nil
	case strings.Contains(err.Error(), "testing value"), strings.Contains(err.Error(), "missing value"):
		// Older servers report a failed test operation as a generic error.
		return nil
	default:
		return fmt.Errorf("failed to remove the hydrate annotation of %s: %w", app.QualifiedName(), err)
	}
}

// requestRefresh asks the application controller for a normal refresh of app by setting the
// refresh annotation (works whichever controller shard owns app). A pending refresh request
// (normal or hard) is left as is.
func (h *Hydrator) requestRefresh(ctx context.Context, app *v1alpha1.Application) error {
	if _, ok := app.Annotations[v1alpha1.AnnotationKeyRefresh]; ok {
		return nil
	}
	patch := map[string]any{"metadata": map[string]any{"annotations": map[string]string{
		v1alpha1.AnnotationKeyRefresh: string(v1alpha1.RefreshTypeNormal),
	}}}
	data, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("failed to encode refresh patch: %w", err)
	}
	if err := h.d.Client.Patch(ctx, app, client.RawPatch(types.MergePatchType, data)); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to request app refresh: %w", err)
	}
	return nil
}

func jsonPointerEscape(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "~", "~0"), "/", "~1")
}

// userinfoPattern matches credentials embedded in URLs ("scheme://user:password@").
var userinfoPattern = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.\-]*://)([^/@\s:]*):([^/@\s]+)@`)

// statusMessage bounds and sanitizes an error message stored in the status: passwords
// embedded in URLs are redacted (repository content and git output are untrusted, and the
// status is readable by every user who can read the Application).
func statusMessage(msg string) string {
	msg = userinfoPattern.ReplaceAllString(msg, "${1}${2}:***@")
	r := []rune(msg)
	if len(r) > maxStatusMessageRunes {
		msg = string(r[:maxStatusMessageRunes]) + "..."
	}
	return msg
}
