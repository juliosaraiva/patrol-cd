package apiserver

import (
	"context"
	"errors"
	"fmt"
	"slices"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/core"
	"github.com/juliosaraiva/patrol-cd/internal/auth"
)

// AccountLookup serves auth.AccountLookup from the current settings snapshot (accounts
// of patrol-cm with passwords and token metadata of patrol-secret).
func AccountLookup(src core.SettingsSource) auth.AccountLookup {
	return auth.AccountLookupFunc(func(_ context.Context, name string) (auth.Account, error) {
		st := src.Current()
		if st == nil {
			return auth.Account{}, errors.New("settings are not loaded yet")
		}
		return st.Accounts().Get(name)
	})
}

// ProjectGetter returns an AppProject of the control-plane namespace by name; a missing
// project is reported with a Kubernetes NotFound error.
type ProjectGetter func(ctx context.Context, name string) (*v1alpha1.AppProject, error)

// ProjectTokenRegistry implements auth.TokenRegistry on AppProject status: a project-role
// token is known while status.jwtTokensByRole[role] lists its ID, or, for tokens recorded
// without an ID, its issue time (upstream TEN-035). Deleting the project, the role or
// the record revokes the token.
func ProjectTokenRegistry(get ProjectGetter) auth.TokenRegistry {
	return auth.TokenRegistryFunc(func(ctx context.Context, subject, jti string, issuedAt int64) (bool, error) {
		project, role, ok := auth.ParseProjectSubject(subject)
		if !ok {
			return false, nil
		}
		p, err := get(ctx, project)
		if err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil
			}
			return false, fmt.Errorf("get project %s: %w", project, err)
		}
		if !slices.ContainsFunc(p.Spec.Roles, func(r v1alpha1.ProjectRole) bool { return r.Name == role }) {
			return false, nil
		}
		for _, t := range p.Status.JWTTokensByRole[role].Items {
			if jti != "" && t.ID == jti {
				return true, nil
			}
			if t.ID == "" && issuedAt != 0 && t.IssuedAt == issuedAt {
				return true, nil
			}
		}
		return false, nil
	})
}
