package deploy_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/server"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// rbacCase is one profile run with exactly its rendered RBAC.
type rbacCase struct {
	profile string
	// api: patrol-server runs and the admin account is enabled (login with the bootstrap
	// password, objects created through the API); otherwise objects are created with the
	// admin kubeconfig (what kubectl or `patrol --core` users do).
	api bool
	// destInRelease deploys to the release namespace (namespaced RBAC); otherwise to
	// <namespace>-dest.
	destInRelease bool
	// inClusterNamespaces registers the local cluster with explicit namespaces (namespaced
	// installs, docs/guides/installation.md).
	inClusterNamespaces []string
	// localCluster: patrol-server has RBAC on the local cluster (live resource views).
	localCluster bool
	// tlsSecrets creates the internalTLS.mode=existingSecret Secrets (test CA).
	tlsSecrets bool
	// defaultProject: nobody creates AppProject "default" in this profile (no patrol-server);
	// the test creates it as the operator would.
	createDefaultProject bool
	// sar are SubjectAccessReview expectations documenting the profile's local-cluster scope.
	sar []sarCheck
	// allow are audit 403s the profile provokes on purpose (documented behavior).
	allow []allowedDenial
	// name overrides the case name (default: profile); values are extra chart values layered
	// over the profile.
	name, values string
	// appNS puts the Applications and the ApplicationSet in this namespace (apps in any
	// namespace); "" uses the release namespace.
	appNS string
}

func (c rbacCase) id() string {
	if c.name != "" {
		return c.name
	}
	return c.profile
}

// sarCheck asks the API server whether a component's service account may do something.
type sarCheck struct {
	sa                               string // service account name
	verb, group, resource, namespace string
	subresource                      string // e.g. "token", "exec" ("" for the resource itself)
	name                             string // object name ("" for any; for list/watch: a metadata.name field selector)
	want                             bool
	why                              string
}

// scenarioRepo is the git content the scenario deploys.
var scenarioRepo = map[string]string{
	"guestbook/cm.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: rbac-probe\ndata:\n  hello: world\n",
	"set/a/cm.yaml":     "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: rbac-set-a\ndata:\n  element: a\n",
	"set/b/cm.yaml":     "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: rbac-set-b\ndata:\n  element: b\n",
}

// TestRBACSufficiency runs the patrolcd binary as every component's ServiceAccount with
// exactly the RBAC the chart renders for a profile, drives a minimal scenario (bootstrap,
// login, repository, Application synced to the local cluster, ApplicationSet with a list
// generator, notifications controller running), and fails on any request the API server
// denied (audit log) or any "forbidden" in the component logs. Rules no request used are
// reported as "unexercised" (informational).
func TestRBACSufficiency(t *testing.T) {
	t.Parallel()
	k := sharedEnv(t)
	helmBin(t)
	cases := []rbacCase{
		{
			profile: "default", api: true, localCluster: true,
			sar: []sarCheck{
				{sa: "patrol-controller", verb: "*", group: "*", resource: "*", want: true, why: "rbac.controller.clusterAdmin (default): the in-cluster destination is cluster-admin by design (Argo CD parity, installation.md)"},
				{sa: "patrol-server", verb: "get", resource: "secrets", namespace: "kube-system", want: true, why: "rbac.server.localCluster (default): live views of any local resource"},
				{sa: "patrol-server", verb: "create", resource: "secrets", namespace: "kube-system", want: false, why: "patrol-server writes on the local cluster are limited to patch/delete"},
				{sa: "patrol-applicationset-controller", verb: "get", resource: "secrets", namespace: "kube-system", want: false, why: "no local-cluster access"},
				{sa: "patrol-notifications-controller", verb: "list", resource: "pods", namespace: "", want: false, why: "no local-cluster access"},
				{sa: "patrol-repo-server", verb: "get", resource: "configmaps", namespace: "", want: false, why: "the repo-server has no Kubernetes access"},
			},
		},
		{
			profile: "core", createDefaultProject: true,
			sar: []sarCheck{
				{sa: "patrol-controller", verb: "*", group: "*", resource: "*", want: true, why: "core profile keeps the cluster-admin controller"},
			},
		},
		{
			profile: "namespaced", api: true, destInRelease: true, localCluster: true, inClusterNamespaces: []string{"@release", "team-a"},
			sar: []sarCheck{
				{sa: "patrol-controller", verb: "create", resource: "configmaps", namespace: "@release", want: true, why: "namespaced: full control of the release namespace"},
				{sa: "patrol-controller", verb: "create", resource: "configmaps", namespace: "team-a", want: true, why: "namespaced: rbac.managedNamespaces"},
				{sa: "patrol-controller", verb: "create", resource: "configmaps", namespace: "default", want: false, why: "namespaced: no access outside the managed namespaces"},
				{sa: "patrol-controller", verb: "list", group: "rbac.authorization.k8s.io", resource: "clusterroles", want: false, why: "namespaced: no cluster-scoped access"},
				{sa: "patrol-server", verb: "get", resource: "secrets", namespace: "default", want: false, why: "namespaced: live views limited to the managed namespaces"},
			},
		},
		{
			profile: "minimal", api: true, localCluster: true,
			sar: []sarCheck{{sa: "patrol-notifications-controller", verb: "get", resource: "secrets", namespace: "@release", want: false, why: "minimal: no notifications controller, no RBAC for it"}},
		},
		{profile: "hydrator", api: true, localCluster: true},
		{profile: "ingress-nginx", api: true, localCluster: true},
		{
			// Apps in any namespace: a namespace pattern makes the Application RBAC of
			// patrol-server, the controller and the notifications controller cluster-wide.
			name: "apps-any-namespace", profile: "default", api: true, localCluster: true, appNS: "rbac-apps-team",
			values: "applicationNamespaces: [\"rbac-apps-*\"]\napplicationSetNamespaces: [rbac-apps-team]\n" +
				"configs:\n  params:\n    applicationsetcontroller.allowed.scm.providers: https://api.github.com/\n",
			sar: []sarCheck{
				{sa: "patrol-server", verb: "create", group: "cd.patrol.example", resource: "applications", namespace: "rbac-apps-other", want: true, why: "a pattern in applicationNamespaces gives cluster-wide Application RBAC (documented)"},
				{sa: "patrol-applicationset-controller", verb: "create", group: "cd.patrol.example", resource: "applicationsets", namespace: "rbac-apps-team", want: true, why: "literal applicationSetNamespaces get a Role"},
				{sa: "patrol-applicationset-controller", verb: "create", group: "cd.patrol.example", resource: "applicationsets", namespace: "rbac-apps-other", want: false, why: "only the listed ApplicationSet namespace"},
			},
		},
		{
			// Self-service notifications with a namespace pattern: the notifications controller
			// reads only the named ConfigMap and Secret in Application namespaces (informers
			// field-selected by metadata.name, cluster-wide), never other Secrets.
			name: "notifications-selfservice", profile: "default", api: true, localCluster: true, appNS: "rbac-ss-team",
			values: "applicationNamespaces: [\"rbac-ss-*\"]\napplicationSetNamespaces: [rbac-ss-team]\n" +
				"configs:\n  params:\n    applicationsetcontroller.allowed.scm.providers: https://api.github.com/\n" +
				"    notificationscontroller.selfservice.enabled: \"true\"\n",
			sar: []sarCheck{
				{sa: "patrol-notifications-controller", verb: "list", resource: "secrets", namespace: "rbac-ss-team", want: false, why: "self-service: no unfiltered Secret list in Application namespaces"},
				{sa: "patrol-notifications-controller", verb: "get", resource: "secrets", namespace: "rbac-ss-other", name: "db-credentials", want: false, why: "self-service: other Secrets of Application namespaces stay unreadable"},
				{sa: "patrol-notifications-controller", verb: "watch", resource: "secrets", name: "patrol-notifications-secret", want: true, why: "self-service: cluster-wide informer field-selected by name"},
				{sa: "patrol-notifications-controller", verb: "list", resource: "configmaps", name: "patrol-notifications-cm", want: true, why: "self-service: cluster-wide informer field-selected by name"},
				{sa: "patrol-notifications-controller", verb: "get", resource: "configmaps", namespace: "rbac-ss-team", name: "patrol-notifications-cm", want: true, why: "self-service: tenant configuration"},
			},
		},
		{
			profile: "hardened", tlsSecrets: true,
			sar: []sarCheck{
				{sa: "patrol-controller", verb: "list", group: "*", resource: "*", want: true, why: "hardened: the live-state cache reads everything"},
				{sa: "patrol-controller", verb: "create", resource: "configmaps", namespace: "default", want: true, why: "hardened: rbac.controller.rules allow core writes"},
				{sa: "patrol-controller", verb: "create", group: "rbac.authorization.k8s.io", resource: "clusterroles", want: false, why: "hardened: no RBAC writes (not in rbac.controller.rules)"},
				{sa: "patrol-controller", verb: "create", group: "rbac.authorization.k8s.io", resource: "clusterrolebindings", want: false, why: "hardened: no RBAC writes"},
				{sa: "patrol-controller", verb: "create", resource: "serviceaccounts", subresource: "token", namespace: "kube-system", want: false, why: "hardened: resources named per group, no subresource writes (installation.md, Hardened installation)"},
				{sa: "patrol-controller", verb: "create", resource: "pods", subresource: "exec", namespace: "default", want: false, why: "hardened: no pods/exec"},
				{sa: "patrol-controller", verb: "patch", resource: "pods", subresource: "ephemeralcontainers", namespace: "default", want: false, why: "hardened: no ephemeral containers"},
				{sa: "patrol-controller", verb: "create", resource: "nodes", subresource: "proxy", want: false, why: "hardened: no nodes/proxy create"},
				{sa: "patrol-controller", verb: "update", resource: "nodes", want: false, why: "hardened: no node writes"},
				{sa: "patrol-controller", verb: "update", resource: "namespaces", want: false, why: "hardened: no namespace writes"},
				{sa: "patrol-controller", verb: "create", group: "admissionregistration.k8s.io", resource: "validatingwebhookconfigurations", want: false, why: "hardened: no admission object writes"},
				{sa: "patrol-controller", verb: "create", resource: "secrets", namespace: "kube-system", want: true, why: "hardened: Secret writes in any namespace remain (documented residual escalation path)"},
				{sa: "patrol-controller", verb: "get", resource: "nodes", subresource: "proxy", want: true, why: "hardened: the read-everything rule includes get on nodes/proxy (documented)"},
				{sa: "patrol-server", verb: "get", resource: "pods", namespace: "default", want: false, why: "hardened: rbac.server.localCluster=false"},
			},
		},
	}
	for _, c := range cases {
		t.Run(c.id(), func(t *testing.T) {
			runRBACCase(t, k, c, nil)
		})
	}
}

// TestRBACMissingRuleDetected proves the harness detects missing permissions: with the
// notifications controller's Lease rules and patrol-server's AppProject/Cluster/Repository
// rule removed from the rendered Roles, both components must be reported (audit 403s and
// "forbidden" in their logs).
func TestRBACMissingRuleDetected(t *testing.T) {
	t.Parallel()
	k := sharedEnv(t)
	helmBin(t)
	c := rbacCase{profile: "default", api: true}
	mutate := func(objs []object) []object {
		for i := range objs {
			o := &objs[i]
			if o.u.GetKind() != "Role" {
				continue
			}
			rules, _, _ := unstructured.NestedSlice(o.u.Object, "rules")
			var keep []any
			for _, r := range rules {
				m := r.(map[string]any)
				groups, _, _ := unstructured.NestedStringSlice(m, "apiGroups")
				res, _, _ := unstructured.NestedStringSlice(m, "resources")
				switch {
				case o.u.GetName() == "patrol-notifications-controller" && slices.Contains(groups, "coordination.k8s.io"):
					continue
				case o.u.GetName() == "patrol-server" && slices.Contains(res, "appprojects"):
					continue
				}
				keep = append(keep, r)
			}
			if len(keep) == len(rules) {
				continue
			}
			if err := unstructured.SetNestedSlice(o.u.Object, keep, "rules"); err != nil {
				t.Fatal(err)
			}
			raw, err := yaml.Marshal(o.u.Object) // apply sends the raw document
			if err != nil {
				t.Fatal(err)
			}
			o.raw = raw
			t.Logf("removed %d rule(s) from Role %s", len(rules)-len(keep), o.u.GetName())
		}
		return objs
	}
	runRBACCase(t, k, c, &inverseOptions{
		mutate: mutate,
		expect: []inverseExpectation{
			{sa: "patrol-notifications-controller", request: regexp.MustCompile(`leases\.coordination\.k8s\.io`), proc: "notifications-controller"},
			{sa: "patrol-server", request: regexp.MustCompile(`appprojects\.cd\.patrol\.example`), proc: "server"},
		},
	})
}

type inverseOptions struct {
	mutate func([]object) []object
	expect []inverseExpectation
}

type inverseExpectation struct {
	sa      string
	request *regexp.Regexp
	proc    string // container name whose log must contain "forbidden"
}

// rbacResult summarizes one run for the run record.
type rbacResult struct {
	analysis rbacAnalysis
	procs    []string
}

var forbiddenRe = regexp.MustCompile(`(?i)forbidden`)

// runRBACCase installs a profile's rendered objects into a fresh namespace, runs the pods and
// the scenario (or, with inv, waits for the expected denials), then analyzes the audit log.
func runRBACCase(t *testing.T, k *kubeEnv, c rbacCase, inv *inverseOptions) rbacResult {
	ns := "rbac-" + c.id()
	if inv != nil {
		ns = "rbac-inverse"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	dest := ns + "-dest"
	if c.destInRelease {
		dest = ns
	}
	valueFiles := []string{filepath.Join(chartDir(), "ci", c.profile+".yaml")}
	if c.values != "" {
		f := filepath.Join(t.TempDir(), "extra.yaml")
		if err := os.WriteFile(f, []byte(c.values), 0o600); err != nil {
			t.Fatal(err)
		}
		valueFiles = append(valueFiles, f)
	}
	objs, err := parseObjects(render(t, releaseName, ns, valueFiles))
	if err != nil {
		t.Fatal(err)
	}
	if inv != nil {
		objs = inv.mutate(objs)
	}
	for _, n := range append(namespacesOf(objs), ns, dest, c.appNS) {
		if n == "" {
			continue
		}
		if err := k.ensureNamespace(ctx, n, nil); err != nil {
			t.Fatal(err)
		}
	}
	// Install like `helm install` would, minus the CRDs and the admission policy, which the
	// shared control plane already has from deploy/crds (TestRenderedCRDsMatchDeployCRDs
	// proves they are identical).
	installed := 0
	rc := k.kube.Discovery().RESTClient()
	for _, o := range objs {
		switch o.u.GetKind() {
		case "CustomResourceDefinition", "ValidatingAdmissionPolicy", "ValidatingAdmissionPolicyBinding":
			continue
		}
		if thirdPartyKinds[o.u.GroupVersionKind()] {
			continue
		}
		if err := k.apply(ctx, rc, o, ns, applyOptions{fieldManager: "deploy-test-install", force: true, strict: true}); err != nil {
			t.Fatal(err)
		}
		installed++
	}
	if c.tlsSecrets {
		createInternalTLSSecrets(ctx, t, k, ns)
	}
	gs := newGitServer(t)
	repoURL := gs.repo(t, "scenario", scenarioRepo)

	run := newPodRunner(t, k, ns, objs)
	if err := run.serviceAccountConfigs(ctx); err != nil {
		t.Fatal(err)
	}
	t.Logf("%s: %d objects installed in %s; %d pods to run (skipped: %v)", c.id(), installed, ns, len(run.pods), run.skipped)
	var wg sync.WaitGroup
	errs := make(chan error, len(run.pods))
	for _, p := range run.pods {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := run.start(ctx, p); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	stopped := false
	stopAll := func() {
		if stopped {
			return
		}
		stopped = true
		for _, p := range stopOrder(run.pods) {
			run.stop(t, p)
		}
	}
	defer stopAll()

	if inv == nil {
		for _, p := range run.pods {
			run.waitReady(t, p, 2*time.Minute)
		}
		scenario(ctx, t, k, run, c, ns, dest, repoURL, gs)
		for _, s := range c.sar {
			checkSAR(ctx, t, k, ns, s)
		}
	} else {
		for _, e := range inv.expect {
			user := "system:serviceaccount:" + ns + ":" + e.sa
			waitFor(t, 2*time.Minute, "a denied request of "+e.sa, func() (bool, string) {
				evs, err := readAudit(k.auditLog, func(u string) bool { return u == user })
				if err != nil {
					return false, err.Error()
				}
				for _, ev := range evs {
					if ev.code() == 403 && e.request.MatchString(ev.describe()) {
						return true, ""
					}
				}
				return false, fmt.Sprintf("%d events, no matching 403", len(evs))
			})
		}
	}
	stopAll()

	// Audit: every request of this namespace's service accounts.
	prefix := "system:serviceaccount:" + ns + ":"
	evs, err := readAudit(k.auditLog, func(u string) bool { return strings.HasPrefix(u, prefix) })
	if err != nil {
		t.Fatal(err)
	}
	rules, err := boundRules(objs, ns)
	if err != nil {
		t.Fatal(err)
	}
	a := analyzeRBAC(evs, rules, c.allow)
	res := rbacResult{analysis: a}
	t.Logf("%s: %d audited requests by %d service accounts %v", c.id(), a.requests, len(a.users), a.users)
	for _, p := range run.allProcs() {
		res.procs = append(res.procs, p.name()+": "+p.commandLine())
		forb := forbiddenLines(p.log.String())
		if inv == nil && len(forb) > 0 {
			t.Errorf("%s logged %d line(s) with \"forbidden\":\n%s", p.name(), len(forb), joinLines(forb))
		}
		if strings.Contains(p.log.String(), "WARNING: DATA RACE") {
			t.Errorf("%s: data race detected:\n%s", p.name(), tail(p.log.String(), 8000))
		}
		if p.stopDur > 0 {
			t.Logf("%s stopped %s after SIGTERM", p.name(), p.stopDur.Round(time.Millisecond))
		}
	}
	if inv == nil {
		for _, f := range a.forbidden {
			t.Errorf("denied request: %s", f)
		}
	} else {
		for _, e := range inv.expect {
			user := prefix + e.sa
			if !slices.ContainsFunc(a.forbidden, func(f string) bool { return strings.HasPrefix(f, user+":") && e.request.MatchString(f) }) {
				t.Errorf("inverse: no denied request of %s matching %s in the analysis:\n%s", e.sa, e.request, joinLines(a.forbidden))
			}
			found := false
			for _, p := range run.allProcs() {
				if p.c.Name == e.proc && len(forbiddenLines(p.log.String())) > 0 {
					found = true
				}
			}
			if !found {
				t.Errorf("inverse: container %s logged no \"forbidden\" line", e.proc)
			}
		}
		for _, f := range a.forbidden {
			t.Logf("detected (expected) denial: %s", f)
		}
	}
	for _, al := range a.allowed403 {
		t.Logf("allowed denial: %s", al)
	}
	t.Logf("%s: %d rule(s) with unexercised verbs/resources (informational):\n%s", c.id(), len(a.unexercised), joinLines(a.unexercised))
	return res
}

// forbiddenLines returns the log lines mentioning "forbidden".
func forbiddenLines(log string) []string {
	var out []string
	for _, line := range strings.Split(log, "\n") {
		if forbiddenRe.MatchString(line) {
			out = append(out, tail(line, 600))
		}
	}
	return out
}

// stopOrder stops clients first and the repo-server last.
func stopOrder(pods []*localPod) []*localPod {
	rank := func(p *localPod) int {
		for _, c := range p.tmpl.Spec.Containers {
			args := append(slices.Clone(c.Command), c.Args...)
			if len(args) < 2 {
				continue
			}
			switch args[1] {
			case "notifications-controller", "appset-controller":
				return 0
			case "server":
				return 1
			case "controller":
				return 2
			case "commit-server":
				return 3
			case "repo-server":
				return 4
			}
		}
		return 5
	}
	out := slices.Clone(pods)
	slices.SortStableFunc(out, func(a, b *localPod) int { return rank(a) - rank(b) })
	return out
}

// createInternalTLSSecrets creates the Secrets of internalTLS.mode=existingSecret, signed by
// a test CA (each with ca.crt, tls.crt, tls.key).
func createInternalTLSSecrets(ctx context.Context, t *testing.T, k *kubeEnv, ns string) {
	t.Helper()
	ca := newTestCA(t)
	for name, cn := range map[string]string{
		"patrol-repo-server-tls":     "patrol-repo-server",
		"patrol-controller-tls":      "patrol-controller",
		"patrol-internal-client-tls": "patrol-internal-client",
		"patrol-commit-server-tls":   "patrol-commit-server",
	} {
		crt, key := ca.issue(t, cn, cn+"."+ns, cn+"."+ns+".svc")
		s := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Type:       corev1.SecretTypeTLS,
			Data:       map[string][]byte{"ca.crt": ca.pem, "tls.crt": crt, "tls.key": key},
		}
		if _, err := k.kube.CoreV1().Secrets(ns).Create(ctx, s, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			t.Fatal(err)
		}
	}
}

// checkSAR asks the API server (SubjectAccessReview) whether a component may do something.
func checkSAR(ctx context.Context, t *testing.T, k *kubeEnv, ns string, s sarCheck) {
	t.Helper()
	target := s.namespace
	if target == "@release" {
		target = ns
	}
	sar := &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
		User:   "system:serviceaccount:" + ns + ":" + s.sa,
		Groups: []string{"system:serviceaccounts", "system:serviceaccounts:" + ns, "system:authenticated"},
		ResourceAttributes: &authorizationv1.ResourceAttributes{
			Verb: s.verb, Group: s.group, Resource: s.resource, Subresource: s.subresource, Namespace: target, Name: s.name,
		},
	}}
	out, err := k.kube.AuthorizationV1().SubjectAccessReviews().Create(ctx, sar, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	res := s.resource
	if s.subresource != "" {
		res += "/" + s.subresource
	}
	if out.Status.Allowed != s.want {
		t.Errorf("SubjectAccessReview %s %s %s.%s %q in %q: allowed=%v, want %v (%s)", s.sa, s.verb, res, s.group, s.name, target, out.Status.Allowed, s.want, s.why)
	} else {
		t.Logf("access as documented: %s %s %s.%s %q in %q allowed=%v (%s)", s.sa, s.verb, res, s.group, s.name, target, s.want, s.why)
	}
}

// scenario drives the components: bootstrap results, login, repository, an Application
// synced by the controller into the local cluster, live views through patrol-server, an
// ApplicationSet with a list generator, and the leader election / shard Leases.
func scenario(ctx context.Context, t *testing.T, k *kubeEnv, run *podRunner, c rbacCase, ns, dest, repoURL string, gs *gitServer) {
	t.Helper()
	// Names are unique per profile: several installations share the control plane and the
	// controllers track resources by Application name (label tracking without an
	// installationID), so equal names would make one installation see another's resources.
	appName, setName := "probe-"+c.id(), "set-"+c.id()
	appNS := ns
	if c.appNS != "" {
		appNS = c.appNS
	}
	kc, err := client.New(k.cfg, client.Options{Scheme: deployScheme})
	if err != nil {
		t.Fatal(err)
	}

	// 1. Bootstrap: the init container created the first-run Secrets.
	tok, err := k.kube.CoreV1().Secrets(ns).Get(ctx, server.InternalTokenSecretName, metav1.GetOptions{})
	if err != nil || len(tok.Data[server.InternalTokenSecretKey]) != 64 {
		t.Fatalf("bootstrap: %s: %v (len %d)", server.InternalTokenSecretName, err, len(tok.Data[server.InternalTokenSecretKey]))
	}
	ps, err := k.kube.CoreV1().Secrets(ns).Get(ctx, settings.DefaultSecretName, metav1.GetOptions{})
	if err != nil || len(ps.Data[settings.SecretKeyServerSecretKey]) == 0 {
		t.Fatalf("bootstrap: %s without server.secretkey: %v", settings.DefaultSecretName, err)
	}
	if c.profile == "core" {
		// Core: no patrol-server, so nothing creates the default project (finding F-core-1).
		var p v1alpha1.AppProject
		if err := kc.Get(ctx, client.ObjectKey{Namespace: ns, Name: "default"}, &p); !apierrors.IsNotFound(err) {
			t.Errorf("core: AppProject default exists (err=%v); the finding about core installs is stale", err)
		}
	}
	if c.createDefaultProject {
		mustCreate(ctx, t, kc, `apiVersion: cd.patrol.example/v1alpha1
kind: AppProject
metadata: {name: default, namespace: `+ns+`}
spec:
  sourceRepos: ["*"]
  destinations: [{server: "*", namespace: "*"}]
  clusterResourceWhitelist: [{group: "*", kind: "*"}]
`)
	}
	if len(c.inClusterNamespaces) > 0 {
		var nss []string
		for _, n := range c.inClusterNamespaces {
			if n == "@release" {
				n = ns
			}
			nss = append(nss, n)
		}
		mustCreate(ctx, t, kc, fmt.Sprintf(`apiVersion: cd.patrol.example/v1alpha1
kind: Cluster
metadata: {name: in-cluster, namespace: %s}
spec:
  name: in-cluster
  server: https://kubernetes.default.svc
  namespaces: [%s]
`, ns, strings.Join(nss, ", ")))
	}

	app := fmt.Sprintf(`apiVersion: cd.patrol.example/v1alpha1
kind: Application
metadata: {name: %s, namespace: %s}
spec:
  project: default
  source: {repoURL: %q, path: guestbook, targetRevision: main}
  destination: {server: https://kubernetes.default.svc, namespace: %s}
  syncPolicy: {automated: {}}
`, appName, appNS, repoURL, dest)
	appset := fmt.Sprintf(`apiVersion: cd.patrol.example/v1alpha1
kind: ApplicationSet
metadata: {name: %s, namespace: %s}
spec:
  generators:
    - list:
        elements: [{name: a}, {name: b}]
  template:
    metadata: {name: "%s-{{name}}"}
    spec:
      project: default
      source: {repoURL: %q, path: "set/{{name}}", targetRevision: main}
      destination: {server: https://kubernetes.default.svc, namespace: %s}
`, setName, appNS, setName, repoURL, dest)

	var api *apiclient.Client
	if c.api {
		// 2. Login with the bootstrap's initial admin password.
		init, err := k.kube.CoreV1().Secrets(ns).Get(ctx, server.InitialAdminSecretName, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("initial admin Secret: %v", err)
		}
		sp := run.pod(releaseName + "-server")
		if sp == nil {
			t.Fatal("no patrol-server pod")
		}
		insecure := slices.Contains(sp.procs[0].args, "--insecure")
		scheme := "https"
		if insecure {
			scheme = "http"
		}
		addr := fmt.Sprintf("%s://127.0.0.1:%d", scheme, sp.ports[8080])
		api, err = apiclient.NewClient(&apiclient.ClientOptions{ServerAddr: addr, Insecure: !insecure, PlainText: insecure, Timeout: time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := api.Login(ctx, "admin", string(init.Data[server.InitialAdminSecretKey])); err != nil {
			t.Fatalf("login as admin with the bootstrap password: %v", err)
		}
		if appNS != ns {
			// Apps in any namespace: the project must list the namespace (operator action).
			waitFor(t, time.Minute, "AppProject default created by patrol-server", func() (bool, string) {
				var p v1alpha1.AppProject
				err := kc.Get(ctx, client.ObjectKey{Namespace: ns, Name: "default"}, &p)
				return err == nil, fmt.Sprint(err)
			})
			patch := []byte(`{"spec":{"sourceNamespaces":["` + appNS + `"]}}`)
			p := &v1alpha1.AppProject{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "default"}}
			if err := kc.Patch(ctx, p, client.RawPatch(types.MergePatchType, patch)); err != nil {
				t.Fatalf("allow %s in project default: %v", appNS, err)
			}
		}
		// 3. Repository with credentials (patrol-server writes the Repository and its Secret).
		if _, err := api.CreateRepository(ctx, &apiclient.RepositoryCreateRequest{Body: apiclient.RepositoryWriteRequest{
			Spec:        v1alpha1.RepositorySpec{URL: repoURL, Type: v1alpha1.RepositoryTypeGit},
			Credentials: &apiclient.RepositoryCredentials{Username: gs.user, Password: gs.password},
		}}); err != nil {
			t.Fatalf("create repository through the API: %v", err)
		}
		// 4. Application through the API.
		var a v1alpha1.Application
		if err := yaml.UnmarshalStrict([]byte(app), &a); err != nil {
			t.Fatal(err)
		}
		if _, err := api.CreateApplication(ctx, &apiclient.ApplicationCreateRequest{Application: &a}); err != nil {
			t.Fatalf("create application through the API: %v", err)
		}
	} else {
		mustCreate(ctx, t, kc, fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata:
  name: scenario-repo
  namespace: %s
  labels: {%s: %s}
stringData: {username: %q, password: %q}
`, ns, v1alpha1.LabelKeySecretType, v1alpha1.LabelValueSecretTypeRepoCreds, gs.user, gs.password))
		mustCreate(ctx, t, kc, fmt.Sprintf(`apiVersion: cd.patrol.example/v1alpha1
kind: RepositoryCredential
metadata: {name: scenario, namespace: %s}
spec:
  urlPrefix: %q
  type: git
  credentialsRef: {name: scenario-repo}
`, ns, gs.url+"/"))
		mustCreate(ctx, t, kc, app)
	}

	// 5. The controller renders the source (repo-server), syncs it into the local cluster
	// with its own identity and reports Synced/Healthy.
	waitFor(t, 4*time.Minute, "Application "+appName+" Synced and Healthy", func() (bool, string) {
		var a v1alpha1.Application
		if err := kc.Get(ctx, client.ObjectKey{Namespace: appNS, Name: appName}, &a); err != nil {
			return false, err.Error()
		}
		var conds []string
		for _, cd := range a.Status.Conditions {
			conds = append(conds, cd.Type+"="+cd.Message)
		}
		return a.Status.Sync.Status == v1alpha1.SyncStatusCodeSynced && a.Status.Health.Status == v1alpha1.HealthStatusHealthy,
			fmt.Sprintf("sync=%s health=%s conditions=%v", a.Status.Sync.Status, a.Status.Health.Status, conds)
	})
	cm, err := k.kube.CoreV1().ConfigMaps(dest).Get(ctx, "rbac-probe", metav1.GetOptions{})
	if err != nil || cm.Data["hello"] != "world" {
		t.Fatalf("deployed ConfigMap %s/rbac-probe: %v", dest, err)
	}
	var ops v1alpha1.OperationList
	if err := kc.List(ctx, &ops, client.InNamespace(appNS), client.MatchingLabels{v1alpha1.LabelKeyOperationApplication: appName}); err != nil {
		t.Fatal(err)
	}
	succeeded := slices.ContainsFunc(ops.Items, func(o v1alpha1.Operation) bool {
		return o.Status.Phase == v1alpha1.OperationSucceeded && o.Spec.InitiatedBy.Automated
	})
	if !succeeded {
		t.Errorf("no succeeded automated Operation for %s (%d Operations)", appName, len(ops.Items))
	}

	// 6. Reads through patrol-server: the tree (controller tree API) and live views (the
	// server's own local-cluster identity).
	if api != nil {
		if _, err := api.GetApplicationResourceTree(ctx, &apiclient.ApplicationTreeQuery{Name: appName, AppNamespace: appNS}); err != nil {
			t.Errorf("resource tree through the API: %v", err)
		}
		if _, err := api.GetApplicationManagedResources(ctx, &apiclient.ApplicationManagedResourcesQuery{Name: appName, AppNamespace: appNS}); err != nil {
			t.Errorf("managed resources through the API: %v", err)
		}
		if c.localCluster {
			live, err := api.GetApplicationResource(ctx, &apiclient.ApplicationResourceRequest{
				Name: appName, AppNamespace: appNS, Namespace: dest, ResourceName: "rbac-probe", Version: "v1", Kind: "ConfigMap",
			})
			if err != nil || !strings.Contains(live.Manifest, "rbac-probe") {
				t.Errorf("live resource through the API: %v", err)
			}
			if _, err := api.ListApplicationEvents(ctx, &apiclient.ApplicationEventsQuery{Name: appName, AppNamespace: appNS}); err != nil {
				t.Errorf("application events through the API: %v", err)
			}
		}
	}

	// 7. ApplicationSet with a list generator: the ApplicationSet controller creates the
	// Applications; the controller compares them.
	if api != nil {
		var as v1alpha1.ApplicationSet
		if err := yaml.UnmarshalStrict([]byte(appset), &as); err != nil {
			t.Fatal(err)
		}
		if _, err := api.CreateApplicationSet(ctx, &apiclient.ApplicationSetCreateRequest{ApplicationSet: &as}); err != nil {
			t.Fatalf("create ApplicationSet through the API: %v", err)
		}
	} else {
		mustCreate(ctx, t, kc, appset)
	}
	for _, name := range []string{setName + "-a", setName + "-b"} {
		waitFor(t, 3*time.Minute, "generated Application "+name+" compared", func() (bool, string) {
			var a v1alpha1.Application
			if err := kc.Get(ctx, client.ObjectKey{Namespace: appNS, Name: name}, &a); err != nil {
				return false, err.Error()
			}
			return a.Status.Sync.Status != "" && a.Status.Sync.Status != v1alpha1.SyncStatusCodeUnknown, "sync=" + string(a.Status.Sync.Status)
		})
	}
	// A manual sync of a generated Application: patrol-server creates the Operation on behalf
	// of the logged-in user ("impersonate" for the admission policy); the controller runs it.
	if api != nil {
		if _, err := api.SyncApplication(ctx, &apiclient.ApplicationSyncRequest{Name: setName + "-a", AppNamespace: appNS}); err != nil {
			t.Fatalf("sync %s-a through the API: %v", setName, err)
		}
		waitFor(t, 3*time.Minute, setName+"-a synced by the manual Operation", func() (bool, string) {
			var a v1alpha1.Application
			if err := kc.Get(ctx, client.ObjectKey{Namespace: appNS, Name: setName + "-a"}, &a); err != nil {
				return false, err.Error()
			}
			_, err := k.kube.CoreV1().ConfigMaps(dest).Get(ctx, "rbac-set-a", metav1.GetOptions{})
			return a.Status.Sync.Status == v1alpha1.SyncStatusCodeSynced && err == nil, fmt.Sprintf("sync=%s configmap err=%v", a.Status.Sync.Status, err)
		})
	}

	// 8. Leases: shard 0 of the controller; leader election of the ApplicationSet and
	// notifications controllers (both run with --enable-leader-election).
	leases := []string{"patrol-controller-shard-0"}
	for _, l := range []struct{ workload, lease string }{
		{releaseName + "-applicationset-controller", "58ac56fa.applicationsets.cd.patrol.example"},
		{releaseName + "-notifications-controller", "patrol-notifications-controller"},
	} {
		if p := run.pod(l.workload); p != nil && slices.Contains(p.procs[0].args, "--enable-leader-election") {
			leases = append(leases, l.lease)
		}
	}
	for _, lease := range leases {
		waitFor(t, time.Minute, "Lease "+lease+" held", func() (bool, string) {
			l, err := k.kube.CoordinationV1().Leases(ns).Get(ctx, lease, metav1.GetOptions{})
			if err != nil {
				return false, err.Error()
			}
			return l.Spec.HolderIdentity != nil && *l.Spec.HolderIdentity != "", "no holder"
		})
	}
	if os.Getenv(envKeepLogs) != "" {
		t.Logf("logs kept in %s", os.Getenv(envKeepLogs))
	}
}

// mustCreate creates an object from YAML with the admin client.
func mustCreate(ctx context.Context, t *testing.T, kc client.Client, doc string) {
	t.Helper()
	u := &unstructured.Unstructured{}
	if err := yaml.Unmarshal([]byte(doc), &u.Object); err != nil {
		t.Fatalf("%v\n%s", err, doc)
	}
	if err := kc.Create(ctx, u); err != nil {
		t.Fatalf("create %s %s: %v", u.GetKind(), u.GetName(), err)
	}
}
