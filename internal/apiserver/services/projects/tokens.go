package projects

import (
	"context"
	"slices"
	"time"
	"unicode"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/util/retry"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	"github.com/juliosaraiva/patrol-cd/internal/project"
	"github.com/juliosaraiva/patrol-cd/internal/rbac"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// Token limits (as for account API tokens).
const (
	maxTokenIDLength = 128
	maxTokenLifetime = 100 * 365 * 24 * time.Hour
)

// authorizeToken enforces the token routes' rule (upstream): projects, update, <project>,
// or membership of one of the role's groups. Callers without projects, update get the
// same denial whether the project or role exists or not.
func (s *Service) authorizeToken(ctx context.Context, projName, roleName string) error {
	rest.AuditFrom(ctx).SetCheck(rbac.ResourceProjects, rbac.ActionUpdate, projName)
	if rest.Can(ctx, rbac.ResourceProjects, rbac.ActionUpdate, projName) {
		return nil
	}
	if groups := rest.IdentityFrom(ctx).Groups(); len(groups) > 0 {
		if p, err := s.getProject(ctx, projName); err == nil {
			if r, _, err := project.GetRoleByName(p, roleName); err == nil && slices.ContainsFunc(r.Groups, func(g string) bool { return slices.Contains(groups, g) }) {
				return nil
			}
		} else if !apierrors.IsNotFound(err) {
			return err
		}
	}
	return rest.Authorize(ctx, rbac.ResourceProjects, rbac.ActionUpdate, projName)
}

func validateTokenID(id string) error {
	if len(id) > maxTokenIDLength {
		return rest.InvalidArgument("token id must be at most %d characters", maxTokenIDLength)
	}
	for _, r := range id {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return rest.InvalidArgument("token id must not contain whitespace or control characters")
		}
	}
	return nil
}

// createToken serves POST /api/v1/projects/{project}/roles/{role}/token (upstream
// ProjectService.CreateToken): the token is minted by the token service (subject
// proj:<project>:<role>, jti, iat, exp when expiresIn > 0) and its metadata appended to
// status.jwtTokensByRole[role] with optimistic concurrency; on a conflict the project is
// re-read and a new token minted.
func (s *Service) createToken(ctx context.Context, req *apiclient.ProjectTokenCreateRequest) (*apiclient.ProjectTokenResponse, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	if s.d.Tokens == nil {
		return nil, rest.Unavailable("project tokens are not available").WithCause(errNotConfigured)
	}
	if err := s.authorizeToken(ctx, req.Project, req.Role); err != nil {
		return nil, err
	}
	if req.ExpiresIn < 0 {
		return nil, rest.InvalidArgument("expiresIn must not be negative")
	}
	if req.ExpiresIn > int64(maxTokenLifetime/time.Second) {
		return nil, rest.InvalidArgument("expiresIn must be at most %d seconds", int64(maxTokenLifetime/time.Second))
	}
	if err := validateTokenID(req.ID); err != nil {
		return nil, err
	}
	unlock := s.locks.Lock(req.Project)
	defer unlock()
	var out *apiclient.ProjectTokenResponse
	var stored *v1alpha1.AppProject
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		p, err := s.getProject(ctx, req.Project)
		if err != nil {
			return err
		}
		if err := project.ValidateProject(p); err != nil {
			return validationError(err, "error validating project: ")
		}
		if _, _, err := project.GetRoleByName(p, req.Role); err != nil {
			return rest.NotFound("%s", project.RoleNotFoundMessage(req.Project, req.Role))
		}
		// Ids must be unique among every recorded token of the role (spec and status).
		merged := p.DeepCopy()
		project.NormalizeJWTTokens(merged)
		if err := project.ValidateJWTTokenID(merged, req.Role, req.ID); err != nil {
			return validationError(err, "")
		}
		iss, err := s.d.Tokens.IssueProjectToken(ctx, req.Project, req.Role, req.ID, time.Duration(req.ExpiresIn)*time.Second)
		if err != nil {
			return rest.InvalidArgument("%v", err)
		}
		meta := v1alpha1.JWTToken{IssuedAt: iss.IssuedAt.Unix(), ID: iss.ID}
		if !iss.ExpiresAt.IsZero() {
			meta.ExpiresAt = iss.ExpiresAt.Unix()
		}
		next := p.DeepCopy()
		if err := project.AddJWTToken(next, req.Role, meta); err != nil {
			return validationError(err, "")
		}
		if err := s.d.Client.Status().Update(ctx, next); err != nil {
			return err
		}
		stored = next
		out = &apiclient.ProjectTokenResponse{Token: iss.Token, ID: meta.ID, IssuedAt: meta.IssuedAt, ExpiresAt: meta.ExpiresAt}
		return nil
	})
	if err != nil {
		return nil, err
	}
	audit(ctx, stored, rest.ReasonResourceCreated, "created token")
	return out, nil
}

// deleteToken serves DELETE /api/v1/projects/{project}/roles/{role}/token/{iat} (upstream
// ProjectService.DeleteToken): the token, located by ?id= when given and else by issued-at
// time, is removed from status.jwtTokensByRole and from the deprecated
// spec.roles[].jwtTokens; removing the record revokes the token. Unknown roles and tokens
// are answered with success. Unlike upstream (TEN-009) a project made invalid by kubectl
// does not block revocation.
func (s *Service) deleteToken(ctx context.Context, req *apiclient.ProjectTokenDeleteRequest) (*apiclient.Empty, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	if err := s.authorizeToken(ctx, req.Project, req.Role); err != nil {
		return nil, err
	}
	if req.IAT < 0 {
		return nil, rest.InvalidArgument("iat must not be negative")
	}
	unlock := s.locks.Lock(req.Project)
	defer unlock()
	var stored *v1alpha1.AppProject
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		stored = nil
		p, err := s.getProject(ctx, req.Project)
		if err != nil {
			return err
		}
		next := p.DeepCopy()
		found, err := project.RemoveJWTToken(next, req.Role, req.IAT, req.ID)
		if err != nil || !found {
			return nil // unknown role or token: nothing to revoke
		}
		// The legacy spec copy goes first: if only the status write succeeded, a later
		// normalization would merge the spec copy back and revive the token.
		if !equality.Semantic.DeepEqual(p.Spec, next.Spec) {
			want := next.Status.DeepCopy()
			if err := s.d.Client.Update(ctx, next); err != nil {
				return err
			}
			next.Status = *want // the update response carries the stored status
		}
		if !equality.Semantic.DeepEqual(p.Status, next.Status) {
			if err := s.d.Client.Status().Update(ctx, next); err != nil {
				return err
			}
		}
		stored = next
		return nil
	})
	if err != nil {
		return nil, err
	}
	if stored != nil {
		audit(ctx, stored, rest.ReasonResourceDeleted, "deleted token")
	}
	return &apiclient.Empty{}, nil
}
