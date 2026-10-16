package compare

import (
	"context"
	"fmt"

	"github.com/juliosaraiva/patrol-cd/internal/diff"
)

// MaskSecrets returns a copy of r.Managed in which core Secret data (and the annotations
// listed in hideAnnotations, resource.sensitive.mask.annotations) are masked in the target,
// the live object and the diff result, so the result can be cached or served (upstream
// hideSecretData). Equal values get equal masks, so changes stay visible. With server-side
// diff the result pair is masked as is; otherwise the diff is recomputed on the masked
// objects (recomputing a server-side diff would resurrect webhook drift).
func (r *Result) MaskSecrets(ctx context.Context, hideAnnotations []string) ([]ManagedResource, error) {
	hide := make(map[string]bool, len(hideAnnotations))
	for _, a := range hideAnnotations {
		hide[a] = true
	}
	out := make([]ManagedResource, len(r.Managed))
	for i, mr := range r.Managed {
		out[i] = mr
		if mr.Key.Group != "" || mr.Key.Kind != "Secret" {
			continue
		}
		target, live, err := diff.HideSecretData(mr.Target, mr.Live, hide)
		if err != nil {
			return nil, fmt.Errorf("error hiding secret data: %w", err)
		}
		out[i].Target, out[i].Live = target, live
		if r.ServerSideDiff && mr.Target != nil && mr.Live != nil {
			masked, err := diff.MaskSecretResult(&mr.Diff, hide)
			if err != nil {
				return nil, fmt.Errorf("error hiding secret data in diff result: %w", err)
			}
			out[i].Diff = *masked
			continue
		}
		if target == nil && live == nil {
			continue
		}
		opts := r.diffOpts
		opts.ServerSide = false
		opts.DryRunner = nil
		d, err := diff.Diff(ctx, target, live, opts)
		if err != nil {
			return nil, fmt.Errorf("error computing the diff of masked secret %s: %w", mr.Key.String(), err)
		}
		out[i].Diff = *d
	}
	return out, nil
}
