package projects_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// TestProjectTokens covers TEN-032..036 and API-034 end to end: token creation by
// projects-update holders and by members of the role's groups, the JWT claims, the
// metadata recorded in status, authentication and project-scoped authorization of the
// token, uniqueness of ids, revocation by id and by issued-at time, idempotent deletes,
// protection of the records from project updates, and concurrent creations.
func TestProjectTokens(t *testing.T) {
	e := newEnv(t, envOptions{})
	ctx := context.Background()
	admin, dave, carol := e.client("admin"), e.client("dave"), e.client("carol")
	_, err := admin.CreateProject(ctx, teamProject("team-a"), false)
	require.NoError(t, err)
	_, err = admin.CreateProject(ctx, teamProject("team-b"), false)
	require.NoError(t, err)

	// dave holds projects, update, team-a.
	before := time.Now().Add(-time.Second)
	tok, err := dave.CreateProjectToken(ctx, &apiclient.ProjectTokenCreateRequest{Project: "team-a", Role: "ci", ID: "build-1", ExpiresIn: 3600, Description: "ci"})
	require.NoError(t, err)
	require.NotEmpty(t, tok.Token)
	assert.Equal(t, "build-1", tok.ID)
	assert.GreaterOrEqual(t, tok.IssuedAt, before.Unix())
	assert.Equal(t, tok.IssuedAt+3600, tok.ExpiresAt)
	claims := jwt.MapClaims{}
	_, _, err = jwt.NewParser().ParseUnverified(tok.Token, claims)
	require.NoError(t, err)
	assert.Equal(t, "proj:team-a:ci", claims["sub"])
	assert.Equal(t, "build-1", claims["jti"])
	assert.Equal(t, "patrol", claims["iss"])
	assert.InDelta(t, float64(tok.ExpiresAt), claims["exp"], 0)

	// The record is in status; the token string is not stored anywhere.
	stored := e.stored("team-a")
	assert.Equal(t, []v1alpha1.JWTToken{{IssuedAt: tok.IssuedAt, ExpiresAt: tok.ExpiresAt, ID: "build-1"}}, stored.Status.JWTTokensByRole["ci"].Items)
	got, err := admin.GetProject(ctx, "team-a")
	require.NoError(t, err)
	assert.Len(t, got.Status.JWTTokensByRole["ci"].Items, 1)
	raw, err := admin.DoRaw(ctx, http.MethodGet, apiclient.PathProject, &apiclient.ProjectRequest{Name: "team-a"}, nil, "")
	require.NoError(t, err)
	var sb strings.Builder
	_, _ = fmt.Fprint(&sb, mustRead(t, raw))
	assert.NotContains(t, sb.String(), tok.Token)

	// The token acts as the role, inside its project only (TEN-020/036).
	ci := e.newClient(tok.Token)
	_, err = ci.GetProject(ctx, "team-a")
	require.NoError(t, err, "implicit projects, get on its own project")
	_, err = ci.GetProject(ctx, "team-b")
	assert.Equal(t, http.StatusForbidden, apiErr(t, err).StatusCode)
	l, err := ci.ListProjects(ctx, nil)
	require.NoError(t, err)
	require.Len(t, l.Items, 1)
	assert.Equal(t, "team-a", l.Items[0].Name)
	_, err = ci.CreateProjectToken(ctx, &apiclient.ProjectTokenCreateRequest{Project: "team-a", Role: "ci"})
	assert.Equal(t, http.StatusForbidden, apiErr(t, err).StatusCode, "a role token cannot mint tokens")

	// Ids are unique per role; unknown roles are NotFound; bad lifetimes rejected.
	_, err = dave.CreateProjectToken(ctx, &apiclient.ProjectTokenCreateRequest{Project: "team-a", Role: "ci", ID: "build-1"})
	ae := apiErr(t, err)
	assert.Equal(t, http.StatusBadRequest, ae.StatusCode)
	assert.Equal(t, "Token id 'build-1' has been used. ", ae.Problem.Detail)
	_, err = dave.CreateProjectToken(ctx, &apiclient.ProjectTokenCreateRequest{Project: "team-a", Role: "nope"})
	ae = apiErr(t, err)
	assert.Equal(t, http.StatusNotFound, ae.StatusCode)
	assert.Equal(t, "project 'team-a' does not have role 'nope'", ae.Problem.Detail)
	_, err = dave.CreateProjectToken(ctx, &apiclient.ProjectTokenCreateRequest{Project: "team-a", Role: "ci", ExpiresIn: -1})
	assert.Equal(t, http.StatusBadRequest, apiErr(t, err).StatusCode)
	_, err = dave.CreateProjectToken(ctx, &apiclient.ProjectTokenCreateRequest{Project: "team-a", Role: "ci", ID: "has space"})
	assert.Equal(t, http.StatusBadRequest, apiErr(t, err).StatusCode)

	// Authorization: group members of the role may mint its tokens without projects,
	// update (TEN-032); others get the same denial whether project and role exist or not.
	sso := e.ssoClient("jane", "ci-admins")
	groupTok, err := sso.CreateProjectToken(ctx, &apiclient.ProjectTokenCreateRequest{Project: "team-a", Role: "ci"})
	require.NoError(t, err)
	assert.Len(t, groupTok.ID, 36, "generated UUID")
	_, err = sso.CreateProjectToken(ctx, &apiclient.ProjectTokenCreateRequest{Project: "team-a", Role: "deploy"})
	assert.Equal(t, http.StatusForbidden, apiErr(t, err).StatusCode, "not a member of deploy's groups")
	_, err = carol.CreateProjectToken(ctx, &apiclient.ProjectTokenCreateRequest{Project: "team-a", Role: "ci"})
	denied := apiErr(t, err)
	_, err = carol.CreateProjectToken(ctx, &apiclient.ProjectTokenCreateRequest{Project: "ghost", Role: "ci"})
	ghost := apiErr(t, err)
	assert.Equal(t, http.StatusForbidden, denied.StatusCode)
	assert.Equal(t, denied.StatusCode, ghost.StatusCode)
	assert.Equal(t, http.StatusForbidden, apiErr(t, carol.DeleteProjectToken(ctx, "team-a", "ci", tok.IssuedAt, "build-1")).StatusCode)
	_, err = dave.CreateProjectToken(ctx, &apiclient.ProjectTokenCreateRequest{Project: "team-b", Role: "ci"})
	assert.Equal(t, http.StatusForbidden, apiErr(t, err).StatusCode)
	_, err = e.client("").CreateProjectToken(ctx, &apiclient.ProjectTokenCreateRequest{Project: "team-a", Role: "ci"})
	assert.Equal(t, http.StatusUnauthorized, apiErr(t, err).StatusCode)

	// Project updates cannot touch token records (status or the deprecated spec copy).
	p, err := admin.GetProject(ctx, "team-a")
	require.NoError(t, err)
	p.Status.JWTTokensByRole = nil
	p.Spec.Roles[0].JWTTokens = []v1alpha1.JWTToken{{ID: "forged", IssuedAt: 1}} //nolint:staticcheck // deprecated field: must be ignored
	_, err = admin.UpdateProject(ctx, p)
	require.NoError(t, err)
	stored = e.stored("team-a")
	assert.Len(t, stored.Status.JWTTokensByRole["ci"].Items, 2)
	assert.Empty(t, stored.Spec.Roles[0].JWTTokens) //nolint:staticcheck // deprecated field
	_, err = ci.GetProject(ctx, "team-a")
	require.NoError(t, err, "still valid")

	// Revocation by id (TEN-033): the token stops authenticating at once.
	require.NoError(t, dave.DeleteProjectToken(ctx, "team-a", "ci", 0, "build-1"))
	_, err = ci.GetProject(ctx, "team-a")
	assert.Equal(t, http.StatusUnauthorized, apiErr(t, err).StatusCode)
	// Idempotent deletes: unknown token, unknown role.
	require.NoError(t, dave.DeleteProjectToken(ctx, "team-a", "ci", 0, "build-1"))
	require.NoError(t, dave.DeleteProjectToken(ctx, "team-a", "nope", 123, ""))

	// Revocation by issued-at time (the CLI passes only the iat).
	groupClient := e.newClient(groupTok.Token)
	_, err = groupClient.GetProject(ctx, "team-a")
	require.NoError(t, err)
	require.NoError(t, sso.DeleteProjectToken(ctx, "team-a", "ci", groupTok.IssuedAt, ""))
	_, err = groupClient.GetProject(ctx, "team-a")
	assert.Equal(t, http.StatusUnauthorized, apiErr(t, err).StatusCode)

	// Legacy records in the deprecated spec copy are removed with the status record, so a
	// later normalization cannot revive the token.
	legacy, err := dave.CreateProjectToken(ctx, &apiclient.ProjectTokenCreateRequest{Project: "team-a", Role: "ci", ID: "legacy"})
	require.NoError(t, err)
	cur := e.stored("team-a")
	cur.Spec.Roles[0].JWTTokens = []v1alpha1.JWTToken{{ID: "legacy", IssuedAt: legacy.IssuedAt}} //nolint:staticcheck // simulate a migrated object
	require.NoError(t, e.kube.Update(ctx, cur))
	require.NoError(t, dave.DeleteProjectToken(ctx, "team-a", "ci", 0, "legacy"))
	cur = e.stored("team-a")
	assert.Empty(t, cur.Spec.Roles[0].JWTTokens) //nolint:staticcheck // deprecated field
	assert.Empty(t, cur.Status.JWTTokensByRole["ci"].Items)

	// Concurrent creations do not lose records (optimistic concurrency with retries).
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := range 8 {
		wg.Go(func() {
			_, err := dave.CreateProjectToken(ctx, &apiclient.ProjectTokenCreateRequest{Project: "team-a", Role: "deploy", ID: fmt.Sprintf("c-%d", i)})
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	assert.Len(t, e.stored("team-a").Status.JWTTokensByRole["deploy"].Items, 8)

	// Removing a role drops its records: its tokens stop verifying, and a role re-created
	// with the same name does not revive them.
	deployTok, err := dave.CreateProjectToken(ctx, &apiclient.ProjectTokenCreateRequest{Project: "team-a", Role: "deploy", ID: "d"})
	require.NoError(t, err)
	p, err = admin.GetProject(ctx, "team-a")
	require.NoError(t, err)
	p.Spec.Roles = p.Spec.Roles[:1]
	_, err = admin.UpdateProject(ctx, p)
	require.NoError(t, err)
	_, err = e.newClient(deployTok.Token).GetProject(ctx, "team-a")
	assert.Equal(t, http.StatusUnauthorized, apiErr(t, err).StatusCode)
	assert.NotContains(t, e.stored("team-a").Status.JWTTokensByRole, "deploy")
	p, err = admin.GetProject(ctx, "team-a")
	require.NoError(t, err)
	p.Spec.Roles = append(p.Spec.Roles, v1alpha1.ProjectRole{Name: "deploy"})
	_, err = admin.UpdateProject(ctx, p)
	require.NoError(t, err)
	_, err = e.newClient(deployTok.Token).GetProject(ctx, "team-a")
	assert.Equal(t, http.StatusUnauthorized, apiErr(t, err).StatusCode)

	// A project made invalid outside the API blocks token creation (upstream) but never
	// revocation.
	keep, err := dave.CreateProjectToken(ctx, &apiclient.ProjectTokenCreateRequest{Project: "team-a", Role: "ci", ID: "keep"})
	require.NoError(t, err)
	cur = e.stored("team-a")
	cur.Spec.SourceRepos = []string{"dup", "dup"}
	require.NoError(t, e.kube.Update(ctx, cur))
	_, err = dave.CreateProjectToken(ctx, &apiclient.ProjectTokenCreateRequest{Project: "team-a", Role: "ci"})
	ae = apiErr(t, err)
	assert.Equal(t, http.StatusBadRequest, ae.StatusCode)
	assert.Equal(t, "error validating project: source repository 'dup' already added", ae.Problem.Detail)
	require.NoError(t, dave.DeleteProjectToken(ctx, "team-a", "ci", keep.IssuedAt, "keep"))
	_, err = e.newClient(keep.Token).GetProject(ctx, "team-a")
	assert.Equal(t, http.StatusUnauthorized, apiErr(t, err).StatusCode)

	// Audit: token creation and deletion are recorded against the project.
	creates := e.audit.byOperation("createProjectToken")
	require.NotEmpty(t, creates)
	assert.Equal(t, "dave created token", creates[0].Message)
	assert.Equal(t, "AppProject", creates[0].Target.Kind)
	var msgs []string
	for _, en := range e.audit.byOperation("deleteProjectToken") {
		msgs = append(msgs, en.Message)
	}
	assert.Contains(t, msgs, "dave deleted token")
	assert.Contains(t, msgs, "jane deleted token")
}

func mustRead(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return sb.String()
}
