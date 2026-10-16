package applications_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/services/applications"
	reposerver "github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

const secretManifest = "apiVersion: v1\nkind: Secret\nmetadata:\n  name: creds\nstringData:\n  password: hunter2\n---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cfg\ndata:\n  a: b\n"

func TestSyncApplication(t *testing.T) {
	e := newEnv(t, envOptions{})
	app := e.newApp("guestbook", "proj-a")
	app.Spec.SyncPolicy = &v1alpha1.SyncPolicy{
		SyncOptions: v1alpha1.SyncOptions{"CreateNamespace=true"},
		Retry:       &v1alpha1.RetryStrategy{Limit: 3},
	}
	stored := e.createApp(app)
	syncer := e.client("syncer")

	resp, err := syncer.SyncApplication(e.ctx, &apiclient.ApplicationSyncRequest{Name: "guestbook", Prune: true,
		Infos: []v1alpha1.Info{{Name: "ticket", Value: "ABC-1"}}})
	require.NoError(t, err)
	require.NotNil(t, resp.Operation)
	require.NotNil(t, resp.Application)
	op := resp.Operation
	assert.Equal(t, "guestbook", op.Spec.Application)
	assert.Equal(t, "syncer", op.Spec.InitiatedBy.Username)
	assert.False(t, op.Spec.InitiatedBy.Automated)
	require.NotNil(t, op.Spec.Sync)
	assert.Equal(t, sha, op.Spec.Sync.Revision, "branch resolved to a commit by the repo-server")
	require.NotNil(t, op.Spec.Sync.Source)
	assert.Equal(t, "guestbook", op.Spec.Sync.Source.Path)
	assert.True(t, op.Spec.Sync.Prune)
	assert.Equal(t, v1alpha1.SyncOptions{"CreateNamespace=true"}, op.Spec.Sync.SyncOptions)
	require.NotNil(t, op.Spec.Retry)
	assert.Equal(t, int64(3), op.Spec.Retry.Limit)
	assert.Equal(t, []v1alpha1.Info{{Name: "ticket", Value: "ABC-1"}}, op.Spec.Info)
	assert.Equal(t, "guestbook", op.Labels[v1alpha1.LabelKeyOperationApplication])
	require.Len(t, op.OwnerReferences, 1)
	assert.Equal(t, stored.UID, op.OwnerReferences[0].UID)
	assert.True(t, *op.OwnerReferences[0].Controller)
	assert.True(t, strings.HasPrefix(op.Name, "guestbook-sync-"))
	res := e.repo.lastResolve()
	require.NotNil(t, res)
	assert.Equal(t, "main", res.Revision)
	assert.True(t, res.ForceRefresh)
	assert.Contains(t, e.events(), "Normal OperationStarted syncer initiated sync to main")
	require.Len(t, e.operations(e.ns, "guestbook"), 1)

	// One operation at a time.
	_, err = syncer.SyncApplication(e.ctx, &apiclient.ApplicationSyncRequest{Name: "guestbook"})
	ae := requireProblem(t, err, http.StatusConflict, "another operation is already in progress")
	assert.Equal(t, apiclient.CodeFailedPrecondition, ae.Code)
	e.finishOperations(e.ns, "guestbook")

	// A commit SHA is not resolved again; request options replace the Application's.
	e.repo.mu.Lock()
	n := len(e.repo.resolves)
	e.repo.mu.Unlock()
	resp, err = syncer.SyncApplication(e.ctx, &apiclient.ApplicationSyncRequest{Name: "guestbook", Revision: sha, DryRun: true,
		SyncOptions:   &apiclient.ApplicationSyncOptions{Items: []string{"ServerSideApply=true"}},
		Strategy:      &v1alpha1.SyncStrategy{Apply: &v1alpha1.SyncStrategyApply{Force: true}},
		Resources:     []v1alpha1.SyncOperationResource{{Kind: "Deployment", Name: "web", Group: "apps"}},
		RetryStrategy: &v1alpha1.RetryStrategy{Limit: 1, Backoff: &v1alpha1.Backoff{Duration: "5s", MaxDuration: "1m"}}})
	require.NoError(t, err)
	e.repo.mu.Lock()
	assert.Equal(t, n, len(e.repo.resolves))
	e.repo.mu.Unlock()
	op = resp.Operation
	assert.Equal(t, sha, op.Spec.Sync.Revision)
	assert.True(t, op.Spec.Sync.DryRun)
	assert.Equal(t, v1alpha1.SyncOptions{"ServerSideApply=true"}, op.Spec.Sync.SyncOptions)
	assert.True(t, op.Spec.Sync.SyncStrategy.Apply.Force)
	assert.Equal(t, int64(1), op.Spec.Retry.Limit)
	assert.Contains(t, e.events(), "Normal OperationStarted syncer initiated partial sync to "+sha)
	e.finishOperations(e.ns, "guestbook")

	// Authorization: get but no sync, other tenants, anonymous.
	_, err = e.client("reader").SyncApplication(e.ctx, &apiclient.ApplicationSyncRequest{Name: "guestbook"})
	requireProblem(t, err, 403, "permission denied: applications, sync, proj-a/guestbook")
	_, err = e.client("bob").SyncApplication(e.ctx, &apiclient.ApplicationSyncRequest{Name: "guestbook"})
	requireProblem(t, err, 403, "permission denied")
	_, err = e.client("").SyncApplication(e.ctx, &apiclient.ApplicationSyncRequest{Name: "guestbook"})
	requireProblem(t, err, 401, "")

	// Request validation.
	for _, tc := range []struct {
		name   string
		req    apiclient.ApplicationSyncRequest
		status int
		detail string
	}{
		{"bad sync option", apiclient.ApplicationSyncRequest{SyncOptions: &apiclient.ApplicationSyncOptions{Items: []string{"bogus"}}}, 400, `sync option "bogus" must have the form Key=Value`},
		{"bad resource", apiclient.ApplicationSyncRequest{Resources: []v1alpha1.SyncOperationResource{{Kind: "Deployment"}}}, 400, "resources to sync need a kind and a name"},
		{"bad backoff", apiclient.ApplicationSyncRequest{RetryStrategy: &v1alpha1.RetryStrategy{Backoff: &v1alpha1.Backoff{Duration: "soon"}}}, 400, `retryStrategy.backoff.duration "soon" is not a duration`},
		{"positions on single source", apiclient.ApplicationSyncRequest{Revisions: []string{"x"}, SourcePositions: []int64{1}}, 400, "can only be used with a multi-source application"},
		{"body name mismatch", apiclient.ApplicationSyncRequest{BodyName: "other"}, 400, "does not match the path name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := tc.req
			req.Name = "guestbook"
			_, err := e.client("alice").SyncApplication(e.ctx, &req)
			requireProblem(t, err, tc.status, tc.detail)
		})
	}
	assert.Empty(t, activeOps(e, "guestbook"))
}

func activeOps(e *testEnv, app string) []v1alpha1.Operation {
	var out []v1alpha1.Operation
	for _, op := range e.operations(e.ns, app) {
		if !op.IsTerminal() {
			out = append(out, op)
		}
	}
	return out
}

func TestSyncLocalManifests(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.createApp(e.newApp("local", "proj-a"))

	_, err := e.client("syncer").SyncApplication(e.ctx, &apiclient.ApplicationSyncRequest{Name: "local", Manifests: []string{secretManifest}})
	requireProblem(t, err, 403, "permission denied: applications, override, proj-a/local")

	resp, err := e.client("alice").SyncApplication(e.ctx, &apiclient.ApplicationSyncRequest{Name: "local", Manifests: []string{secretManifest}})
	require.NoError(t, err)
	// Secret data is masked in the response; the stored Operation keeps the manifests.
	shown := resp.Operation.Spec.Sync.Manifests[0]
	assert.NotContains(t, shown, "hunter2")
	assert.Contains(t, shown, "++++++++")
	assert.Contains(t, shown, "kind: ConfigMap")
	assert.Empty(t, resp.Operation.Spec.Sync.Revision)
	stored := e.operations(e.ns, "local")
	require.Len(t, stored, 1)
	assert.Contains(t, stored[0].Spec.Sync.Manifests[0], "hunter2")
	assert.Contains(t, e.events(), "Normal OperationStarted alice initiated sync locally")
	got, err := e.client("reader").GetApplicationOperation(e.ctx, &apiclient.ApplicationOperationRequest{Name: "local", Operation: stored[0].Name})
	require.NoError(t, err)
	assert.NotContains(t, got.Spec.Sync.Manifests[0], "hunter2")
	e.finishOperations(e.ns, "local")

	// Size cap.
	big := strings.Repeat("a", applications.MaxLocalManifestsBytes+1)
	_, err = e.client("alice").SyncApplication(e.ctx, &apiclient.ApplicationSyncRequest{Name: "local", Manifests: []string{big}})
	requireProblem(t, err, 413, "")

	// Automated sync: only dry runs may use local manifests.
	e.updateApp("local", func(a *v1alpha1.Application) {
		a.Spec.SyncPolicy = &v1alpha1.SyncPolicy{Automated: &v1alpha1.SyncPolicyAutomated{}}
	})
	_, err = e.client("alice").SyncApplication(e.ctx, &apiclient.ApplicationSyncRequest{Name: "local", Manifests: []string{"{}"}})
	requireProblem(t, err, http.StatusPreconditionFailed, "cannot use local sync when Automatic Sync Policy is enabled unless for dry run")
	_, err = e.client("alice").SyncApplication(e.ctx, &apiclient.ApplicationSyncRequest{Name: "local", Manifests: []string{"{}"}, DryRun: true})
	require.NoError(t, err)
	e.finishOperations(e.ns, "local")

	// Source integrity forbids local manifests.
	e.updateProject("proj-a", func(p *v1alpha1.AppProject) {
		p.Spec.SourceIntegrity = &v1alpha1.SourceIntegrity{Git: &v1alpha1.SourceIntegrityGit{Policies: []v1alpha1.SourceIntegrityGitPolicy{{
			Repos: []v1alpha1.SourceIntegrityGitPolicyRepo{{URL: "*"}},
			GPG:   &v1alpha1.SourceIntegrityGitPolicyGPG{Mode: v1alpha1.SourceIntegrityGitPolicyGPGModeHead, Keys: []string{"4AEE18F83AFDEB23"}},
		}}}}
	})
	_, err = e.client("alice").SyncApplication(e.ctx, &apiclient.ApplicationSyncRequest{Name: "local", Manifests: []string{"{}"}, DryRun: true})
	requireProblem(t, err, http.StatusPreconditionFailed, "Cannot use local manifests when source integrity is enforced")
}

func TestSyncRevisionOverride(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.createApp(e.newApp("ov", "proj-a"))
	syncer, alice := e.client("syncer"), e.client("alice")

	// Without the setting, any syncer may pick a revision.
	resp, err := syncer.SyncApplication(e.ctx, &apiclient.ApplicationSyncRequest{Name: "ov", Revision: "feature"})
	require.NoError(t, err)
	assert.Equal(t, "feature", e.repo.lastResolve().Revision)
	assert.Equal(t, sha, resp.Operation.Spec.Sync.Revision)
	e.finishOperations(e.ns, "ov")

	e.holder.setCM("application.sync.requireOverridePrivilegeForRevisionSync", "true")
	_, err = syncer.SyncApplication(e.ctx, &apiclient.ApplicationSyncRequest{Name: "ov", Revision: "feature"})
	requireProblem(t, err, 403, "permission denied: applications, override, proj-a/ov")
	// The target revision itself (and HEAD for an empty target) is not an override.
	_, err = syncer.SyncApplication(e.ctx, &apiclient.ApplicationSyncRequest{Name: "ov", Revision: "main"})
	require.NoError(t, err)
	e.finishOperations(e.ns, "ov")
	_, err = alice.SyncApplication(e.ctx, &apiclient.ApplicationSyncRequest{Name: "ov", Revision: "feature"})
	require.NoError(t, err)
	e.finishOperations(e.ns, "ov")

	// Automated sync: overrides only as dry runs.
	e.updateApp("ov", func(a *v1alpha1.Application) {
		a.Spec.SyncPolicy = &v1alpha1.SyncPolicy{Automated: &v1alpha1.SyncPolicyAutomated{}}
	})
	_, err = alice.SyncApplication(e.ctx, &apiclient.ApplicationSyncRequest{Name: "ov", Revision: "feature"})
	requireProblem(t, err, http.StatusPreconditionFailed, "Cannot sync to feature: auto-sync currently set to main")
	_, err = alice.SyncApplication(e.ctx, &apiclient.ApplicationSyncRequest{Name: "ov", Revision: "feature", DryRun: true})
	require.NoError(t, err)
	e.finishOperations(e.ns, "ov")

	// Unknown revision.
	e.repo.mu.Lock()
	e.repo.resolveErr = &reposerver.Error{Code: reposerver.CodeInvalidArgument, Message: "unable to resolve 'nope' to a commit SHA"}
	e.repo.mu.Unlock()
	_, err = alice.SyncApplication(e.ctx, &apiclient.ApplicationSyncRequest{Name: "ov", Revision: "nope", DryRun: true})
	requireProblem(t, err, http.StatusPreconditionFailed, "unable to resolve 'nope' to a commit SHA")
	// An unavailable repo-server is 503, not a precondition failure.
	e.repo.mu.Lock()
	e.repo.resolveErr = &reposerver.Error{Code: reposerver.CodeUnavailable, Message: "connection refused"}
	e.repo.mu.Unlock()
	_, err = alice.SyncApplication(e.ctx, &apiclient.ApplicationSyncRequest{Name: "ov", DryRun: true})
	requireProblem(t, err, http.StatusServiceUnavailable, "the repo-server is unavailable")
	assert.Empty(t, activeOps(e, "ov"))
}

func TestSyncMultiSourceAndOverrides(t *testing.T) {
	e := newEnv(t, envOptions{})
	ms := e.newApp("multi", "proj-a")
	ms.Spec.Source = nil
	ms.Spec.Sources = v1alpha1.ApplicationSources{
		{RepoURL: repoURL, Path: "base", TargetRevision: "main"},
		{RepoURL: "https://github.com/org/values", TargetRevision: "v1", Ref: "values"},
	}
	e.createApp(ms)
	e.repo.mu.Lock()
	e.repo.resolved = map[string]string{"v2": strings.Repeat("b", 40)}
	e.repo.mu.Unlock()

	resp, err := e.client("syncer").SyncApplication(e.ctx, &apiclient.ApplicationSyncRequest{Name: "multi", Revisions: []string{"v2"}, SourcePositions: []int64{2}})
	require.NoError(t, err)
	op := resp.Operation
	assert.Nil(t, op.Spec.Sync.Source)
	require.Len(t, op.Spec.Sync.Sources, 2)
	assert.Equal(t, []string{sha, strings.Repeat("b", 40)}, op.Spec.Sync.Revisions)
	assert.Empty(t, op.Spec.Sync.Revision)
	assert.Contains(t, e.events(), "Normal OperationStarted syncer initiated sync to main,v2")
	e.finishOperations(e.ns, "multi")

	for _, tc := range []struct {
		req    apiclient.ApplicationSyncRequest
		detail string
	}{
		{apiclient.ApplicationSyncRequest{Revisions: []string{"x"}, SourcePositions: []int64{3}}, "source position is out of range"},
		{apiclient.ApplicationSyncRequest{Revisions: []string{"x"}, SourcePositions: []int64{0}}, "source position is out of range"},
		{apiclient.ApplicationSyncRequest{Revisions: []string{"x", "y"}, SourcePositions: []int64{1}}, "revisions and sourcePositions must have the same number of values"},
		{apiclient.ApplicationSyncRequest{Revision: "x"}, "revision cannot be used with a multi-source application"},
	} {
		req := tc.req
		req.Name = "multi"
		_, err := e.client("alice").SyncApplication(e.ctx, &req)
		requireProblem(t, err, 400, tc.detail)
	}

	// Source overrides need override, and the project must permit them.
	single := e.createApp(e.newApp("single", "proj-a"))
	override := single.Spec.Source.DeepCopy()
	override.Path = "other"
	_, err = e.client("syncer").SyncApplication(e.ctx, &apiclient.ApplicationSyncRequest{Name: "single", Source: override})
	requireProblem(t, err, 403, "permission denied: applications, override, proj-a/single")
	evil := single.Spec.Source.DeepCopy()
	evil.RepoURL = "https://gitlab.com/evil/repo"
	_, err = e.client("alice").SyncApplication(e.ctx, &apiclient.ApplicationSyncRequest{Name: "single", Source: evil})
	requireProblem(t, err, 400, "sync source override is invalid: InvalidSpecError: application repo https://gitlab.com/evil/repo is not permitted in project 'proj-a'")
	_, err = e.client("alice").SyncApplication(e.ctx, &apiclient.ApplicationSyncRequest{Name: "single", Sources: v1alpha1.ApplicationSources{*override}})
	requireProblem(t, err, 400, "a single-source application takes source, not sources")
	_, err = e.client("alice").SyncApplication(e.ctx, &apiclient.ApplicationSyncRequest{Name: "single", Source: override, Manifests: []string{"{}"}})
	requireProblem(t, err, 400, "manifests and source overrides are mutually exclusive")
	resp, err = e.client("alice").SyncApplication(e.ctx, &apiclient.ApplicationSyncRequest{Name: "single", Source: override})
	require.NoError(t, err)
	assert.Equal(t, "other", resp.Operation.Spec.Sync.Source.Path)
	e.finishOperations(e.ns, "single")
	// The spec source itself is not an override.
	_, err = e.client("syncer").SyncApplication(e.ctx, &apiclient.ApplicationSyncRequest{Name: "single", Source: single.Spec.Source})
	require.NoError(t, err)
}

func TestSyncPreconditions(t *testing.T) {
	e := newEnv(t, envOptions{config: func(_ *testEnv, c *applicationsConfig) { c.DisableSyncWithReplace = true }})
	e.createApp(e.newApp("pre", "proj-a"))
	alice := e.client("alice")

	_, err := alice.SyncApplication(e.ctx, &apiclient.ApplicationSyncRequest{Name: "pre", SyncOptions: &apiclient.ApplicationSyncOptions{Items: []string{"Replace=true"}}})
	requireProblem(t, err, http.StatusPreconditionFailed, "sync with replace was disabled on the API Server level via the server configuration")

	// A deny window blocks manual syncs unless manualSync is set.
	e.updateProject("proj-a", func(p *v1alpha1.AppProject) {
		p.Spec.SyncWindows = v1alpha1.SyncWindows{{Kind: "deny", Schedule: "* * * * *", Duration: "1h", Applications: []string{"pre"}}}
	})
	_, err = alice.SyncApplication(e.ctx, &apiclient.ApplicationSyncRequest{Name: "pre"})
	requireProblem(t, err, 403, "cannot sync: blocked by sync window")
	// Callers without sync learn nothing about windows.
	_, err = e.client("reader").SyncApplication(e.ctx, &apiclient.ApplicationSyncRequest{Name: "pre"})
	requireProblem(t, err, 403, "permission denied: applications, sync")
	e.updateProject("proj-a", func(p *v1alpha1.AppProject) { p.Spec.SyncWindows[0].ManualSync = true })
	_, err = alice.SyncApplication(e.ctx, &apiclient.ApplicationSyncRequest{Name: "pre"})
	require.NoError(t, err)
	e.finishOperations(e.ns, "pre")

	// An Application being deleted.
	e.updateApp("pre", func(a *v1alpha1.Application) { a.Finalizers = []string{"example.com/hold"} })
	require.NoError(t, e.kube.Delete(e.ctx, e.getApp(e.ns, "pre")))
	_, err = alice.SyncApplication(e.ctx, &apiclient.ApplicationSyncRequest{Name: "pre"})
	requireProblem(t, err, http.StatusPreconditionFailed, "application is deleting")
	_, err = alice.RollbackApplication(e.ctx, &apiclient.ApplicationRollbackRequest{Name: "pre", ID: 0})
	requireProblem(t, err, http.StatusPreconditionFailed, "application is deleting")
}

func TestSyncRepositoryCredentials(t *testing.T) {
	e := newEnv(t, envOptions{})
	require.NoError(t, e.kube.Create(e.ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "repo-creds", Namespace: e.ns, Labels: map[string]string{v1alpha1.LabelKeySecretType: v1alpha1.LabelValueSecretTypeRepository}},
		StringData: map[string]string{"username": "git", "password": "s3cr3t-token"},
	}))
	require.NoError(t, e.kube.Create(e.ctx, &v1alpha1.Repository{
		ObjectMeta: metav1.ObjectMeta{Name: "repo", Namespace: e.ns},
		Spec:       v1alpha1.RepositorySpec{URL: repoURL, RepositoryAccess: v1alpha1.RepositoryAccess{CredentialsRef: &v1alpha1.LocalSecretReference{Name: "repo-creds"}}},
	}))
	e.createApp(e.newApp("cred", "proj-a"))
	// The resolver's informers see the Repository shortly.
	require.Eventually(t, func() bool {
		resp, err := e.client("alice").SyncApplication(e.ctx, &apiclient.ApplicationSyncRequest{Name: "cred", DryRun: true})
		if err != nil {
			return false
		}
		e.finishOperations(e.ns, "cred")
		return strings.Contains(toJSON(t, resp), "cred") && e.repo.lastResolve().Repo.Password == "s3cr3t-token"
	}, 10*time.Second, 100*time.Millisecond)
	// Credentials reach the repo-server but never a client response.
	resp, err := e.client("alice").GetApplicationManifests(e.ctx, &apiclient.ApplicationManifestQuery{Name: "cred"})
	require.NoError(t, err)
	assert.NotContains(t, toJSON(t, resp), "s3cr3t-token")
	assert.Equal(t, "s3cr3t-token", e.repo.lastGenerate().Repo.Password)
}

func TestRollbackApplication(t *testing.T) {
	e := newEnv(t, envOptions{})
	app := e.createApp(e.newApp("rb", "proj-a"))
	app.Status.History = v1alpha1.RevisionHistories{
		{ID: 0, Revision: "aaa", DeployedAt: metav1.Now(), Source: &v1alpha1.ApplicationSource{RepoURL: repoURL, Path: "guestbook", TargetRevision: "aaa"}},
		{ID: 1, Revision: "bbb", DeployedAt: metav1.Now()},
	}
	require.NoError(t, e.kube.Status().Update(e.ctx, app))

	resp, err := e.client("syncer").RollbackApplication(e.ctx, &apiclient.ApplicationRollbackRequest{Name: "rb", ID: 0, Prune: true})
	require.NoError(t, err)
	op := resp.Operation
	require.NotNil(t, op.Spec.Rollback)
	assert.Equal(t, int64(0), op.Spec.Rollback.ID)
	require.NotNil(t, op.Spec.Sync)
	assert.True(t, op.Spec.Sync.Prune)
	require.NotNil(t, op.Spec.Sync.SyncStrategy.Apply)
	assert.Nil(t, op.Spec.Sync.Source)
	assert.Nil(t, op.Spec.Retry)
	assert.Equal(t, "syncer", op.Spec.InitiatedBy.Username)
	assert.True(t, strings.HasPrefix(op.Name, "rb-rollback-"))
	assert.Contains(t, e.events(), "Normal OperationStarted syncer initiated rollback to 0")

	_, err = e.client("syncer").RollbackApplication(e.ctx, &apiclient.ApplicationRollbackRequest{Name: "rb", ID: 0})
	requireProblem(t, err, http.StatusConflict, "another operation is already in progress")
	e.finishOperations(e.ns, "rb")

	_, err = e.client("syncer").RollbackApplication(e.ctx, &apiclient.ApplicationRollbackRequest{Name: "rb", ID: 7})
	requireProblem(t, err, 400, "application "+e.ns+"/rb does not have deployment with id 7")
	_, err = e.client("syncer").RollbackApplication(e.ctx, &apiclient.ApplicationRollbackRequest{Name: "rb", ID: 1})
	requireProblem(t, err, http.StatusPreconditionFailed, "cannot rollback to a deployment without recorded sources")
	_, err = e.client("reader").RollbackApplication(e.ctx, &apiclient.ApplicationRollbackRequest{Name: "rb", ID: 0})
	requireProblem(t, err, 403, "permission denied")
	_, err = e.client("bob").RollbackApplication(e.ctx, &apiclient.ApplicationRollbackRequest{Name: "rb", ID: 0})
	requireProblem(t, err, 403, "permission denied")

	e.updateApp("rb", func(a *v1alpha1.Application) {
		a.Spec.SyncPolicy = &v1alpha1.SyncPolicy{Automated: &v1alpha1.SyncPolicyAutomated{}}
	})
	_, err = e.client("syncer").RollbackApplication(e.ctx, &apiclient.ApplicationRollbackRequest{Name: "rb", ID: 0})
	requireProblem(t, err, http.StatusPreconditionFailed, "rollback cannot be initiated when auto-sync is enabled")
	// automated.enabled=false keeps the settings but allows a rollback.
	e.updateApp("rb", func(a *v1alpha1.Application) { a.Spec.SyncPolicy.Automated.Enabled = ptrBool(false) })
	_, err = e.client("syncer").RollbackApplication(e.ctx, &apiclient.ApplicationRollbackRequest{Name: "rb", ID: 0})
	require.NoError(t, err)

	// Multi-source Applications roll back only to multi-source entries.
	ms := e.newApp("rbm", "proj-a")
	ms.Spec.Source = nil
	ms.Spec.Sources = v1alpha1.ApplicationSources{{RepoURL: repoURL, Path: "a"}, {RepoURL: repoURL, Path: "b"}}
	ms = e.createApp(ms)
	ms.Status.History = v1alpha1.RevisionHistories{
		{ID: 3, Revision: "x", DeployedAt: metav1.Now(), Source: &v1alpha1.ApplicationSource{RepoURL: repoURL, Path: "a"}},
		{ID: 4, Revisions: []string{"x", "y"}, DeployedAt: metav1.Now(), Sources: ms.Spec.Sources},
	}
	require.NoError(t, e.kube.Status().Update(e.ctx, ms))
	_, err = e.client("syncer").RollbackApplication(e.ctx, &apiclient.ApplicationRollbackRequest{Name: "rbm", ID: 3})
	requireProblem(t, err, http.StatusPreconditionFailed, "cannot rollback a multi-source application to a single-source deployment")
	_, err = e.client("syncer").RollbackApplication(e.ctx, &apiclient.ApplicationRollbackRequest{Name: "rbm", ID: 4})
	require.NoError(t, err)
}

func TestTerminateOperation(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.createApp(e.newApp("term", "proj-a"))
	syncer := e.client("syncer")

	_, err := syncer.TerminateApplicationOperation(e.ctx, &apiclient.ApplicationRef{Name: "term"})
	requireProblem(t, err, 400, "Unable to terminate operation. No operation is in progress")

	_, err = syncer.SyncApplication(e.ctx, &apiclient.ApplicationSyncRequest{Name: "term"})
	require.NoError(t, err)
	_, err = e.client("reader").TerminateApplicationOperation(e.ctx, &apiclient.ApplicationRef{Name: "term"})
	requireProblem(t, err, 403, "permission denied")
	_, err = e.client("bob").TerminateApplicationOperation(e.ctx, &apiclient.ApplicationRef{Name: "term"})
	requireProblem(t, err, 403, "permission denied")

	resp, err := syncer.TerminateApplicationOperation(e.ctx, &apiclient.ApplicationRef{Name: "term"})
	require.NoError(t, err)
	require.NotNil(t, resp.Operation)
	ops := e.operations(e.ns, "term")
	require.Len(t, ops, 1)
	assert.True(t, ops[0].Spec.Cancel)
	assert.Contains(t, e.events(), "Normal ResourceUpdated syncer terminated running operation")
	// Idempotent while the controller is cancelling.
	_, err = syncer.TerminateApplicationOperation(e.ctx, &apiclient.ApplicationRef{Name: "term"})
	require.NoError(t, err)
}

func TestListAndGetOperations(t *testing.T) {
	e := newEnv(t, envOptions{})
	app := e.createApp(e.newApp("ops", "proj-a"))
	other := e.createApp(e.newApp("others", "proj-a"))
	mk := func(a *v1alpha1.Application, name string) {
		op := &v1alpha1.Operation{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: e.ns},
			Spec:       v1alpha1.OperationSpec{Application: a.Name, Sync: &v1alpha1.SyncOperation{Manifests: []string{secretManifest}}},
		}
		require.NoError(t, e.kube.Create(e.ctx, op))
		time.Sleep(1100 * time.Millisecond) // distinct creation timestamps (second precision)
	}
	mk(app, "op-1")
	mk(other, "foreign")
	mk(app, "op-2")
	mk(app, "op-3")

	reader := e.client("reader")
	l, err := reader.ListApplicationOperations(e.ctx, &apiclient.ApplicationOperationsRequest{Name: "ops"})
	require.NoError(t, err)
	var names []string
	for _, op := range l.Items {
		names = append(names, op.Name)
		assert.NotContains(t, op.Spec.Sync.Manifests[0], "hunter2")
	}
	assert.Equal(t, []string{"op-3", "op-2", "op-1"}, names)

	l, err = reader.ListApplicationOperations(e.ctx, &apiclient.ApplicationOperationsRequest{Name: "ops", Limit: 2})
	require.NoError(t, err)
	require.Len(t, l.Items, 2)
	require.NotEmpty(t, l.Continue)
	l, err = reader.ListApplicationOperations(e.ctx, &apiclient.ApplicationOperationsRequest{Name: "ops", Limit: 2, Continue: l.Continue})
	require.NoError(t, err)
	require.Len(t, l.Items, 1)
	assert.Equal(t, "op-1", l.Items[0].Name)
	assert.Empty(t, l.Continue)

	got, err := reader.GetApplicationOperation(e.ctx, &apiclient.ApplicationOperationRequest{Name: "ops", Operation: "op-2"})
	require.NoError(t, err)
	assert.Equal(t, "op-2", got.Name)
	_, err = reader.GetApplicationOperation(e.ctx, &apiclient.ApplicationOperationRequest{Name: "ops", Operation: "foreign"})
	requireProblem(t, err, 404, `operations.cd.patrol.example "foreign" not found`)
	_, err = reader.GetApplicationOperation(e.ctx, &apiclient.ApplicationOperationRequest{Name: "ops", Operation: "missing"})
	requireProblem(t, err, 404, "")
	_, err = e.client("bob").ListApplicationOperations(e.ctx, &apiclient.ApplicationOperationsRequest{Name: "ops"})
	requireProblem(t, err, 403, "permission denied")
	_, err = reader.ListApplicationOperations(e.ctx, &apiclient.ApplicationOperationsRequest{Name: "ops", Limit: 5000})
	requireProblem(t, err, 400, "limit must be between")
}

func TestSyncWindowsState(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.createApp(e.newApp("win", "proj-a"))
	e.updateProject("proj-a", func(p *v1alpha1.AppProject) {
		p.Spec.SyncWindows = v1alpha1.SyncWindows{
			{Kind: "deny", Schedule: "* * * * *", Duration: "1h", Applications: []string{"win"}},
			{Kind: "allow", Schedule: "0 0 1 1 *", Duration: "1m", Applications: []string{"other"}},
		}
	})
	st, err := e.client("reader").GetApplicationSyncWindows(e.ctx, &apiclient.ApplicationRef{Name: "win"})
	require.NoError(t, err)
	assert.False(t, st.CanSync)
	assert.Equal(t, "Sync Denied", st.Status)
	require.Len(t, st.AssignedWindows, 1)
	require.Len(t, st.ActiveWindows, 1)
	assert.Equal(t, "deny", st.ActiveWindows[0].Kind)

	e.updateProject("proj-a", func(p *v1alpha1.AppProject) { p.Spec.SyncWindows[0].ManualSync = true })
	st, err = e.client("reader").GetApplicationSyncWindows(e.ctx, &apiclient.ApplicationRef{Name: "win"})
	require.NoError(t, err)
	assert.True(t, st.CanSync)
	assert.Equal(t, "Manual Allowed", st.Status)

	_, err = e.client("bob").GetApplicationSyncWindows(e.ctx, &apiclient.ApplicationRef{Name: "win"})
	requireProblem(t, err, 403, "permission denied")
}

// updateApp modifies a stored Application directly (as kubectl would).
func (e *testEnv) updateApp(name string, mutate func(*v1alpha1.Application)) {
	e.t.Helper()
	a := e.getApp(e.ns, name)
	mutate(a)
	require.NoError(e.t, e.kube.Update(e.ctx, a))
}

var _ = client.ObjectKey{}
