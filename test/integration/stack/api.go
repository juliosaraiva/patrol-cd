package stack

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// ServerURL is patrol-server's base URL (plain HTTP on loopback).
func (s *Stack) ServerURL() string { return "http://" + s.serverAddr }

// ServerAddr is patrol-server's host:port.
func (s *Stack) ServerAddr() string { return s.serverAddr }

// ClientWithToken returns an API client that sends token (empty: anonymous).
func (s *Stack) ClientWithToken(t testing.TB, token string) *apiclient.Client {
	t.Helper()
	c, err := apiclient.NewClient(&apiclient.ClientOptions{ServerAddr: s.ServerURL(), PlainText: true, AuthToken: token, Timeout: Timeout(2 * time.Minute)})
	if err != nil {
		t.Fatalf("API client: %v", err)
	}
	return c
}

// Login returns an API client with a session of a local account.
func (s *Stack) Login(t testing.TB, user, password string) *apiclient.Client {
	t.Helper()
	c := s.ClientWithToken(t, "")
	ctx, cancel := context.WithTimeout(t.Context(), Timeout(30*time.Second))
	defer cancel()
	if _, err := c.Login(ctx, user, password); err != nil {
		t.Fatalf("login as %s: %v", user, err)
	}
	return c
}

// API returns the admin's API client (one session per Stack).
func (s *Stack) API(t testing.TB) *apiclient.Client {
	t.Helper()
	s.mu.Lock()
	c := s.admin
	s.mu.Unlock()
	if c != nil {
		return c
	}
	c = s.Login(t, "admin", s.AdminPassword)
	s.mu.Lock()
	s.admin = c
	s.mu.Unlock()
	return c
}

// AccountClient logs in a local account of Options.Accounts.
func (s *Stack) AccountClient(t testing.TB, name string) *apiclient.Client {
	t.Helper()
	pw, ok := s.opts.Accounts[name]
	if !ok {
		t.Fatalf("account %s is not configured in Options.Accounts", name)
	}
	return s.Login(t, name, pw)
}

// AppSource returns a directory source of repo at path, tracking branch main.
func AppSource(repo *Repo, path string) *v1alpha1.ApplicationSource {
	return &v1alpha1.ApplicationSource{RepoURL: repo.URL, Path: path, TargetRevision: "main"}
}

// NewApp returns an Application of project default in the control-plane namespace that
// deploys repo/path to namespace of destination d (by server URL).
func (s *Stack) NewApp(name string, repo *Repo, path string, d *Cluster, namespace string) *v1alpha1.Application {
	return &v1alpha1.Application{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: s.Namespace},
		Spec: v1alpha1.ApplicationSpec{
			Project:     "default",
			Source:      AppSource(repo, path),
			Destination: v1alpha1.ApplicationDestination{Server: d.Server, Namespace: namespace},
		},
	}
}

// CreateApp creates an Application through the API (with validation) as admin.
func (s *Stack) CreateApp(t testing.TB, app *v1alpha1.Application) *v1alpha1.Application {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), Timeout(time.Minute))
	defer cancel()
	out, err := s.API(t).CreateApplication(ctx, &apiclient.ApplicationCreateRequest{Application: app})
	if err != nil {
		t.Fatalf("create application %s: %v", app.Name, err)
	}
	return out
}

// GetApp reads an Application of the control-plane namespace from the API server.
func (s *Stack) GetApp(t testing.TB, name string) *v1alpha1.Application {
	t.Helper()
	var app v1alpha1.Application
	if err := s.control.Client.Get(t.Context(), client.ObjectKey{Namespace: s.Namespace, Name: name}, &app); err != nil {
		t.Fatalf("get application %s: %v", name, err)
	}
	return &app
}

// AppGone reports whether the Application no longer exists.
func (s *Stack) AppGone(t testing.TB, name string) bool {
	t.Helper()
	_, ok := s.LookupApp(t, name)
	return !ok
}

// LookupApp returns the Application, or false when it no longer exists. A check that it is gone
// followed by a GetApp would fail when the deletion completes in between.
func (s *Stack) LookupApp(t testing.TB, name string) (*v1alpha1.Application, bool) {
	t.Helper()
	var app v1alpha1.Application
	err := s.control.Client.Get(t.Context(), client.ObjectKey{Namespace: s.Namespace, Name: name}, &app)
	if apierrors.IsNotFound(err) {
		return nil, false
	}
	if err != nil {
		t.Fatalf("get application %s: %v", name, err)
	}
	return &app, true
}

// UpdateApp re-reads an Application, applies mutate and writes it (conflicts retried).
func (s *Stack) UpdateApp(t testing.TB, name string, mutate func(*v1alpha1.Application)) *v1alpha1.Application {
	t.Helper()
	var out *v1alpha1.Application
	Eventually(t, 30*time.Second, "update application "+name, func() (bool, string) {
		app := s.GetApp(t, name)
		mutate(app)
		err := s.control.Client.Update(t.Context(), app)
		if apierrors.IsConflict(err) {
			return false, err.Error()
		}
		if err != nil {
			t.Fatalf("update application %s: %v", name, err)
		}
		out = app
		return true, ""
	})
	return out
}

// DescribeApp summarizes an Application's state for failure messages.
func DescribeApp(app *v1alpha1.Application) string {
	var b strings.Builder
	fmt.Fprintf(&b, "sync=%s health=%s revision=%s", app.Status.Sync.Status, app.Status.Health.Status, short(app.Status.Sync.Revision))
	if op := app.Status.OperationState; op != nil {
		fmt.Fprintf(&b, " operation=%s/%s %q", op.OperationName, op.Phase, op.Message)
	}
	for _, c := range app.Status.Conditions {
		if c.Status == metav1.ConditionTrue && c.Type != v1alpha1.ApplicationConditionReconciled && c.Type != v1alpha1.ApplicationConditionSynced && c.Type != v1alpha1.ApplicationConditionHealthy {
			fmt.Fprintf(&b, " condition %s: %s", c.Type, c.Message)
		}
	}
	var res []string
	for _, r := range app.Status.Resources {
		h := ""
		if r.Health != nil {
			h = "/" + string(r.Health.Status)
		}
		res = append(res, fmt.Sprintf("%s/%s:%s%s", r.Kind, r.Name, r.Status, h))
	}
	sort.Strings(res)
	if len(res) > 0 {
		fmt.Fprintf(&b, " resources=[%s]", strings.Join(res, " "))
	}
	return b.String()
}

func short(rev string) string {
	if len(rev) > 8 {
		return rev[:8]
	}
	return rev
}

// WaitApp polls the Application until cond holds and returns it.
func (s *Stack) WaitApp(t testing.TB, name, what string, cond func(*v1alpha1.Application) bool) *v1alpha1.Application {
	t.Helper()
	var app v1alpha1.Application
	Eventually(t, 3*time.Minute, "application "+name+": "+what, func() (bool, string) {
		if err := s.control.Client.Get(t.Context(), client.ObjectKey{Namespace: s.Namespace, Name: name}, &app); err != nil {
			return false, err.Error()
		}
		return cond(&app), DescribeApp(&app)
	})
	return &app
}

// WaitSyncedHealthy waits until the Application is Synced and Healthy at revision (any
// revision when empty).
func (s *Stack) WaitSyncedHealthy(t testing.TB, name, revision string) *v1alpha1.Application {
	t.Helper()
	return s.WaitApp(t, name, "Synced and Healthy at "+short(revision), func(a *v1alpha1.Application) bool {
		return a.Status.Sync.Status == v1alpha1.SyncStatusCodeSynced && a.Status.Health.Status == v1alpha1.HealthStatusHealthy &&
			(revision == "" || a.Status.Sync.Revision == revision)
	})
}

// WaitSyncStatus waits for a sync status (and revision when not empty).
func (s *Stack) WaitSyncStatus(t testing.TB, name string, status v1alpha1.SyncStatusCode, revision string) *v1alpha1.Application {
	t.Helper()
	return s.WaitApp(t, name, string(status)+" at "+short(revision), func(a *v1alpha1.Application) bool {
		return a.Status.Sync.Status == status && (revision == "" || a.Status.Sync.Revision == revision)
	})
}

// Condition returns the Application condition of a type, or nil.
func Condition(app *v1alpha1.Application, typ string) *metav1.Condition {
	return meta.FindStatusCondition(app.Status.Conditions, typ)
}

// HasCondition reports whether a condition of typ is True (and its message contains want).
func HasCondition(app *v1alpha1.Application, typ, want string) bool {
	c := Condition(app, typ)
	return c != nil && c.Status == metav1.ConditionTrue && strings.Contains(c.Message, want)
}

// Refresh requests a normal (or hard) refresh through the API and waits for the bounded
// refresh wait of the server.
func (s *Stack) Refresh(t testing.TB, name string, hard bool) *v1alpha1.Application {
	t.Helper()
	typ := string(v1alpha1.RefreshTypeNormal)
	if hard {
		typ = string(v1alpha1.RefreshTypeHard)
	}
	ctx, cancel := context.WithTimeout(t.Context(), Timeout(time.Minute))
	defer cancel()
	app, err := s.API(t).GetApplication(ctx, &apiclient.ApplicationGetRequest{Name: name, Refresh: typ})
	if err != nil {
		t.Fatalf("refresh application %s: %v", name, err)
	}
	return app
}

// Sync requests a sync through the admin API and returns the created Operation.
func (s *Stack) Sync(t testing.TB, req *apiclient.ApplicationSyncRequest) *v1alpha1.Operation {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), Timeout(time.Minute))
	defer cancel()
	resp, err := s.API(t).SyncApplication(ctx, req)
	if err != nil {
		t.Fatalf("sync application %s: %v", req.Name, err)
	}
	if resp.Operation == nil {
		t.Fatalf("sync application %s: no operation in the response", req.Name)
	}
	return resp.Operation
}

// GetOperation reads an Operation of the control-plane namespace.
func (s *Stack) GetOperation(t testing.TB, name string) *v1alpha1.Operation {
	t.Helper()
	var op v1alpha1.Operation
	if err := s.control.Client.Get(t.Context(), client.ObjectKey{Namespace: s.Namespace, Name: name}, &op); err != nil {
		t.Fatalf("get operation %s: %v", name, err)
	}
	return &op
}

// DescribeOperation summarizes an Operation for failure messages.
func DescribeOperation(op *v1alpha1.Operation) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s phase=%s retries=%d message=%q", op.Name, op.Status.Phase, op.Status.RetryCount, op.Status.Message)
	if p := op.Status.Progress; p != nil {
		fmt.Fprintf(&b, " progress=%+v", *p)
	}
	if r := op.Status.SyncResult; r != nil {
		for _, res := range r.Resources {
			fmt.Fprintf(&b, " [%s/%s %s %s/%s %q]", res.Kind, res.Name, res.SyncPhase, res.Status, res.HookPhase, res.Message)
		}
	}
	return b.String()
}

// WaitOperation polls an Operation until cond holds.
func (s *Stack) WaitOperation(t testing.TB, name, what string, cond func(*v1alpha1.Operation) bool) *v1alpha1.Operation {
	t.Helper()
	var op v1alpha1.Operation
	Eventually(t, 3*time.Minute, "operation "+name+": "+what, func() (bool, string) {
		if err := s.control.Client.Get(t.Context(), client.ObjectKey{Namespace: s.Namespace, Name: name}, &op); err != nil {
			return false, err.Error()
		}
		return cond(&op), DescribeOperation(&op)
	})
	return &op
}

// WaitOperationPhase waits until an Operation reaches one of phases.
func (s *Stack) WaitOperationPhase(t testing.TB, name string, phases ...v1alpha1.OperationPhase) *v1alpha1.Operation {
	t.Helper()
	return s.WaitOperation(t, name, fmt.Sprintf("phase in %v", phases), func(op *v1alpha1.Operation) bool {
		for _, p := range phases {
			if op.Status.Phase == p {
				return true
			}
		}
		return false
	})
}

// Operations lists the Operations of an Application, oldest first.
func (s *Stack) Operations(t testing.TB, app string) []v1alpha1.Operation {
	t.Helper()
	var list v1alpha1.OperationList
	if err := s.control.Client.List(t.Context(), &list, client.InNamespace(s.Namespace)); err != nil {
		t.Fatalf("list operations: %v", err)
	}
	var out []v1alpha1.Operation
	for _, op := range list.Items {
		if op.Spec.Application == app {
			out = append(out, op)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		ti, tj := out[i].CreationTimestamp, out[j].CreationTimestamp
		if !ti.Equal(&tj) {
			return ti.Before(&tj)
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// Tree reads an Application's resource tree through the API.
func (s *Stack) Tree(t testing.TB, name string) *apiclient.ApplicationTree {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), Timeout(30*time.Second))
	defer cancel()
	tree, err := s.API(t).GetApplicationResourceTree(ctx, &apiclient.ApplicationTreeQuery{Name: name})
	if err != nil {
		t.Fatalf("resource tree of %s: %v", name, err)
	}
	return tree
}

// CompleteJob marks a Job as succeeded (status subresource), as the Job controller would.
func CompleteJob(t testing.TB, c *Cluster, namespace, name string) {
	t.Helper()
	if err := c.setStatus(t.Context(), "batch/v1", "Job", namespace, name, func(map[string]any) map[string]any { return jobStatus(true) }); err != nil {
		t.Fatalf("complete job %s/%s: %v", namespace, name, err)
	}
}

// FailJob marks a Job as failed.
func FailJob(t testing.TB, c *Cluster, namespace, name string) {
	t.Helper()
	if err := c.setStatus(t.Context(), "batch/v1", "Job", namespace, name, func(map[string]any) map[string]any { return jobStatus(false) }); err != nil {
		t.Fatalf("fail job %s/%s: %v", namespace, name, err)
	}
}

// jobStatus is the status the Job controller writes for a finished Job.
func jobStatus(succeeded bool) map[string]any {
	now := time.Now().UTC().Format(time.RFC3339)
	cond := func(typ, reason, msg string) map[string]any {
		return map[string]any{"type": typ, "status": "True", "reason": reason, "message": msg, "lastProbeTime": now, "lastTransitionTime": now}
	}
	if succeeded {
		const msg = "Reached expected number of succeeded pods"
		return map[string]any{
			"startTime": now, "completionTime": now, "succeeded": int64(1), "ready": int64(0), "terminating": int64(0),
			"uncountedTerminatedPods": map[string]any{},
			"conditions":              []any{cond("SuccessCriteriaMet", "CompletionsReached", msg), cond("Complete", "CompletionsReached", msg)},
		}
	}
	const msg = "Job has reached the specified backoff limit"
	return map[string]any{
		"startTime": now, "failed": int64(1), "ready": int64(0), "terminating": int64(0),
		"uncountedTerminatedPods": map[string]any{},
		"conditions":              []any{cond("FailureTarget", "BackoffLimitExceeded", msg), cond("Failed", "BackoffLimitExceeded", msg)},
	}
}

// RolloutDeployment marks a Deployment's current generation as rolled out and available.
func RolloutDeployment(t testing.TB, c *Cluster, namespace, name string) {
	t.Helper()
	if err := c.setStatus(t.Context(), "apps/v1", "Deployment", namespace, name, deploymentRolledOut); err != nil {
		t.Fatalf("roll out deployment %s/%s: %v", namespace, name, err)
	}
}

// deploymentRolledOut is the status of a Deployment whose generation is fully available.
func deploymentRolledOut(obj map[string]any) map[string]any {
	u := unstructured.Unstructured{Object: obj}
	replicas, found, err := unstructured.NestedInt64(obj, "spec", "replicas")
	if !found || err != nil {
		replicas = 1
	}
	now := time.Now().UTC().Format(time.RFC3339)
	cond := func(typ, reason, msg string) map[string]any {
		return map[string]any{"type": typ, "status": "True", "reason": reason, "message": msg, "lastUpdateTime": now, "lastTransitionTime": now}
	}
	return map[string]any{
		"observedGeneration": u.GetGeneration(), "replicas": replicas, "updatedReplicas": replicas,
		"readyReplicas": replicas, "availableReplicas": replicas,
		"conditions": []any{
			cond("Available", "MinimumReplicasAvailable", "Deployment has minimum availability."),
			cond("Progressing", "NewReplicaSetAvailable", "ReplicaSet has successfully progressed."),
		},
	}
}

// HoldAnnotation on a Job or Deployment keeps SimulateWorkloads from completing it, so a
// test can hold a sync wave (release it with CompleteJob or RolloutDeployment).
const HoldAnnotation = "test.patrol.example/hold"

// SimulateWorkloads stands in for the Job and Deployment controllers in a namespace of d
// until the end of the test: Jobs without a terminal condition are completed and
// Deployments whose generation is not observed are rolled out, unless they carry
// HoldAnnotation.
func (s *Stack) SimulateWorkloads(d *Cluster, namespace string) {
	go func() {
		tick := time.NewTicker(300 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-s.ctx.Done():
				return
			case <-tick.C:
			}
			simulateOnce(s.ctx, d, namespace)
		}
	}()
}

func simulateOnce(ctx context.Context, d *Cluster, namespace string) {
	jobs, err := d.Dynamic.Resource(schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "jobs"}).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err == nil {
		for _, j := range jobs.Items {
			if j.GetDeletionTimestamp() != nil || j.GetAnnotations()[HoldAnnotation] != "" || jobFinished(j.Object) {
				continue
			}
			_ = d.setStatus(ctx, "batch/v1", "Job", namespace, j.GetName(), func(map[string]any) map[string]any { return jobStatus(true) })
		}
	}
	deps, err := d.Dynamic.Resource(schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err == nil {
		for _, dep := range deps.Items {
			if dep.GetDeletionTimestamp() != nil || dep.GetAnnotations()[HoldAnnotation] != "" {
				continue
			}
			observed, _, _ := unstructured.NestedInt64(dep.Object, "status", "observedGeneration")
			if observed >= dep.GetGeneration() {
				continue
			}
			_ = d.setStatus(ctx, "apps/v1", "Deployment", namespace, dep.GetName(), deploymentRolledOut)
		}
	}
}

func jobFinished(obj map[string]any) bool {
	conds, _, _ := unstructured.NestedSlice(obj, "status", "conditions")
	for _, c := range conds {
		m, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if t := m["type"]; t == "Complete" || t == "Failed" {
			return true
		}
	}
	return false
}

// RequestRefresh sets the refresh annotation of an Application directly (what the API and
// webhooks do) without waiting for the controller.
func (s *Stack) RequestRefresh(t testing.TB, name string, hard bool) {
	t.Helper()
	typ := v1alpha1.RefreshTypeNormal
	if hard {
		typ = v1alpha1.RefreshTypeHard
	}
	s.control.MergePatch(t, v1alpha1.GroupVersion.String(), "Application", s.Namespace, name,
		fmt.Sprintf(`{"metadata":{"annotations":{%q:%q}}}`, v1alpha1.AnnotationKeyRefresh, typ))
}

// RefreshUntil requests refreshes (one at a time, through the refresh annotation) until cond
// holds: for states the controller only reaches on a new comparison, e.g. after an outage
// (the live-state cache retries a failed sync only after its retry timeout, and the
// repo-server caches branch resolutions).
func (s *Stack) RefreshUntil(t testing.TB, name, what string, cond func(*v1alpha1.Application) bool) *v1alpha1.Application {
	t.Helper()
	var app *v1alpha1.Application
	Eventually(t, 3*time.Minute, "application "+name+": "+what, func() (bool, string) {
		app = s.GetApp(t, name)
		if cond(app) {
			return true, ""
		}
		if _, pending := app.Annotations[v1alpha1.AnnotationKeyRefresh]; !pending {
			s.RequestRefresh(t, name, false)
		}
		return false, DescribeApp(app)
	})
	return app
}
