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
		return projectTokenKnown(ctx, get, subject, jti, issuedAt)
	})
}

// ProjectTokenRegistryWithFallback is ProjectTokenRegistry for a getter that can be stale (an
// informer cache). A token the cached view does not list is looked up again with live before it
// is declared revoked: a token is recorded by one request and first used by another, often on
// another replica, before that replica's cache has seen the write, and a token that works as
// soon as it is returned must not be refused as revoked. A token the cached view lists is
// accepted without the second read; a revoked one costs one live read per attempt.
func ProjectTokenRegistryWithFallback(cached, live ProjectGetter) auth.TokenRegistry {
	return auth.TokenRegistryFunc(func(ctx context.Context, subject, jti string, issuedAt int64) (bool, error) {
		known, err := projectTokenKnown(ctx, cached, subject, jti, issuedAt)
		if err != nil || known {
			return known, err
		}
		return projectTokenKnown(ctx, live, subject, jti, issuedAt)
	})
}

func projectTokenKnown(ctx context.Context, get ProjectGetter, subject, jti string, issuedAt int64) (bool, error) {
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
}
