package deploy_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/juliosaraiva/patrol-cd/internal/auth"
	"github.com/juliosaraiva/patrol-cd/internal/server"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
)

// helmCLI runs helm against an envtest control plane (admin kubeconfig, isolated helm home).
type helmCLI struct {
	t    *testing.T
	bin  string
	env  []string
	kube *kubeEnv
}

func newHelmCLI(t *testing.T, k *kubeEnv) *helmCLI {
	return &helmCLI{t: t, bin: helmBin(t), env: helmEnv(t, "KUBECONFIG="+k.kubeconfig), kube: k}
}

// run executes helm and fails the test on error unless wantErr.
func (h *helmCLI) run(wantErr bool, args ...string) string {
	h.t.Helper()
	r := runCmd(h.t, 5*time.Minute, h.env, h.bin, args...)
	h.t.Logf("helm %s → err=%v", strings.Join(args, " "), r.err)
	if (r.err != nil) != wantErr {
		h.t.Fatalf("helm %s: err=%v (want error: %v)\n%s", strings.Join(args, " "), r.err, wantErr, r.combined())
	}
	return r.combined()
}

// status returns the release status and revision.
func (h *helmCLI) status(release, ns string) (string, int) {
	h.t.Helper()
	out := h.run(false, "status", release, "-n", ns, "-o", "json")
	var s struct {
		Info struct {
			Status string `json:"status"`
		} `json:"info"`
		Version int `json:"version"`
	}
	if err := json.Unmarshal([]byte(out), &s); err != nil {
		h.t.Fatalf("helm status output: %v\n%s", err, out)
	}
	return s.Info.Status, s.Version
}

// get reads an object (nil when it does not exist).
func (k *kubeEnv) get(ctx context.Context, o object, namespace string) (*unstructured.Unstructured, error) {
	m, err := k.mapping(o.u.GroupVersionKind())
	if err != nil {
		return nil, err
	}
	ns := o.u.GetNamespace()
	if ns == "" {
		ns = namespace
	}
	var got *unstructured.Unstructured
	if namespaced, _ := k.isNamespaced(o); namespaced {
		got, err = k.dyn.Resource(m.Resource).Namespace(ns).Get(ctx, o.u.GetName(), metav1.GetOptions{})
	} else {
		got, err = k.dyn.Resource(m.Resource).Get(ctx, o.u.GetName(), metav1.GetOptions{})
	}
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	return got, err
}

// presence counts the rendered objects that exist (or, with gone, that do not).
func presence(ctx context.Context, t *testing.T, k *kubeEnv, objs []object, ns string, wantPresent bool) int {
	t.Helper()
	n := 0
	for _, o := range objs {
		got, err := k.get(ctx, o, ns)
		if err != nil {
			t.Fatalf("%s: %v", o.id(), err)
		}
		if (got != nil) != wantPresent {
			state := "missing"
			if got != nil {
				state = "still present"
			}
			t.Errorf("%s: %s", o.id(), state)
			continue
		}
		n++
	}
	return n
}

func nested(t *testing.T, u *unstructured.Unstructured, fields ...string) any {
	t.Helper()
	v, _, err := unstructured.NestedFieldNoCopy(u.Object, fields...)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// TestHelmLifecycle installs, upgrades (no-op, values and image tag, a component disabled),
// rolls back, uninstalls and re-installs the chart with the helm CLI against a fresh envtest
// API server (no kubelet: --no-hooks, no --wait), and installs a second, namespaced release.
// After the re-install the bootstrap replaces the stale initial admin password (finding DV-6).
func TestHelmLifecycle(t *testing.T) {
	t.Parallel()
	k := freshEnv(t, envOptions{name: "helm"})
	h := newHelmCLI(t, k)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	const ns, release = "lifecycle", "patrol"
	defaults := filepath.Join(chartDir(), "ci", "default.yaml")
	common := []string{"-n", ns, "--no-hooks", "--timeout", "3m"}

	objs, err := parseObjects(render(t, release, ns, []string{defaults}))
	if err != nil {
		t.Fatal(err)
	}
	var crds, clusterScoped, namespaced []object
	for _, o := range objs {
		switch {
		case o.u.GetKind() == "CustomResourceDefinition" || strings.HasPrefix(o.u.GetKind(), "ValidatingAdmissionPolicy"):
			crds = append(crds, o)
		case o.u.GetNamespace() == "":
			clusterScoped = append(clusterScoped, o)
		default:
			namespaced = append(namespaced, o)
		}
	}

	// 0. A typo inside a free-form Kubernetes fragment of the values passes values.schema.json
	// but helm install refuses it (the API server's schema knows no such field).
	typo := filepath.Join(t.TempDir(), "typo.yaml")
	if err := os.WriteFile(typo, []byte("global:\n  containerSecurityContext:\n    readOnlyRootFileSystem: false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Helm 4 applies object by object, so the failed install is partial: the kept CRDs now
	// carry this release's ownership. The same release name and namespace are used so the
	// real install below can adopt them after the failed release is uninstalled.
	out := h.run(true, "install", release, chartDir(), "-f", defaults, "-f", typo, "-n", ns, "--create-namespace", "--no-hooks", "--timeout", "3m")
	if !strings.Contains(out, "readOnlyRootFileSystem") {
		t.Errorf("helm install with a securityContext typo failed for another reason:\n%s", out)
	}
	t.Logf("typo refused by helm install: %s", lastLine(out))
	if st, _ := h.status(release, ns); st != "failed" {
		t.Errorf("release after the refused install: status %s, want failed", st)
	}
	h.run(false, "uninstall", release, "-n", ns, "--no-hooks")

	// 1. Install.
	h.run(false, append([]string{"install", release, chartDir(), "-f", defaults, "--create-namespace"}, common...)...)
	if st, rev := h.status(release, ns); st != "deployed" || rev != 1 {
		t.Fatalf("after install: status %s revision %d", st, rev)
	}
	n := presence(ctx, t, k, objs, ns, true)
	t.Logf("install: %d/%d rendered objects present", n, len(objs))

	snapshot := func() map[string]*unstructured.Unstructured {
		out := map[string]*unstructured.Unstructured{}
		for _, o := range byKind(objs, "Deployment", "StatefulSet", "Service", "ConfigMap") {
			got, err := k.get(ctx, o, ns)
			if err != nil {
				t.Fatal(err)
			}
			if got != nil {
				out[o.id()] = got
			}
		}
		return out
	}
	// versions records, for every rendered object, a value that any write changes: the
	// resourceVersion, except for the kinds whose status the API server itself writes after
	// creation (CRD conditions, the admission policy's type checking), which are compared by
	// generation (spec changes) plus labels and annotations.
	versions := func() map[string]string {
		out := map[string]string{}
		for _, o := range objs {
			got, err := k.get(ctx, o, ns)
			switch {
			case err != nil:
				t.Fatal(err)
			case got == nil:
				out[o.id()] = "missing"
			case got.GetKind() == "CustomResourceDefinition" || got.GetKind() == "ValidatingAdmissionPolicy":
				out[o.id()] = fmt.Sprintf("generation %d, labels %v, annotations %v", got.GetGeneration(), got.GetLabels(), got.GetAnnotations())
			default:
				out[o.id()] = "resourceVersion " + got.GetResourceVersion()
			}
		}
		return out
	}
	// The bootstrap init container (patrolcd bootstrap, run here with the admin kubeconfig as
	// no pod starts) adds the generated keys and seeds the default known hosts at runtime;
	// upgrades and rollbacks must keep them. The snapshot follows its writes.
	bootstrapRun(t, k, ns)
	before := snapshot()
	beforeVersions := versions()
	keys := runtimeSecrets(ctx, t, k, ns)
	// The first initial admin Secret: its UID and password (kept in memory, never logged) are
	// compared after uninstall + re-install (step 8).
	firstInitial, err := k.kube.CoreV1().Secrets(ns).Get(ctx, server.InitialAdminSecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	firstPassword := string(firstInitial.Data[server.InitialAdminSecretKey])
	if firstPassword == "" {
		t.Fatalf("install: %s has no %s", server.InitialAdminSecretName, server.InitialAdminSecretKey)
	}
	if err := adminLogin(ctx, t, k, ns, firstPassword); err != nil {
		t.Fatalf("install: the initial admin password does not log in: %v", err)
	}
	checkKeys := func(step string) {
		t.Helper()
		got := runtimeSecrets(ctx, t, k, ns)
		for name, want := range keys {
			if got[name] != want {
				t.Errorf("%s changed runtime Secret material %s", step, name)
			}
		}
	}

	// 2. Upgrade with identical values: no rendered object changes (idempotent upgrade).
	h.run(false, append([]string{"upgrade", release, chartDir(), "-f", defaults}, common...)...)
	if st, rev := h.status(release, ns); st != "deployed" || rev != 2 {
		t.Fatalf("after no-op upgrade: status %s revision %d", st, rev)
	}
	unchanged := 0
	for id, got := range versions() {
		if b := beforeVersions[id]; b != got {
			t.Errorf("no-op upgrade modified %s (%s → %s)", id, b, got)
			continue
		}
		unchanged++
	}
	t.Logf("no-op upgrade: %d/%d rendered objects unchanged", unchanged, len(objs))
	checkKeys("no-op upgrade")

	// 3. Upgrade with changed values, a new image tag, common labels (selectors must not
	// change: they are immutable) and a disabled component.
	h.run(false, append([]string{"upgrade", release, chartDir(), "-f", defaults,
		"--set", "image.tag=0.1.1", "--set", "server.replicas=2", "--set-string", "global.commonLabels.team=platform",
		"--set", "controller.resources.limits.memory=3Gi", "--set", "notificationsController.enabled=false"}, common...)...)
	if st, rev := h.status(release, ns); st != "deployed" || rev != 3 {
		t.Fatalf("after upgrade: status %s revision %d", st, rev)
	}
	after := snapshot()
	for id, b := range before {
		got := after[id]
		if strings.Contains(id, "notifications") {
			continue // disabled by this upgrade
		}
		if got == nil {
			t.Errorf("upgrade removed %s", id)
			continue
		}
		switch b.GetKind() {
		case "Deployment", "StatefulSet":
			if !reflect.DeepEqual(nested(t, b, "spec", "selector"), nested(t, got, "spec", "selector")) {
				t.Errorf("%s: selector changed by the upgrade", id)
			}
			if got.GetLabels()["team"] != "platform" {
				t.Errorf("%s: commonLabels not applied", id)
			}
			for _, c := range nested(t, got, "spec", "template", "spec", "containers").([]any) {
				img := c.(map[string]any)["image"].(string)
				if strings.HasPrefix(img, "patrol-cd/patrol-cd") && !strings.HasSuffix(img, ":0.1.1") {
					t.Errorf("%s: image %s not updated to 0.1.1", id, img)
				}
			}
		case "Service":
			if nested(t, b, "spec", "clusterIP") != nested(t, got, "spec", "clusterIP") {
				t.Errorf("%s: clusterIP changed (%v → %v)", id, nested(t, b, "spec", "clusterIP"), nested(t, got, "spec", "clusterIP"))
			}
		}
	}
	checkKeys("upgrade")
	srv := after["Deployment/"+ns+"/patrol-server"]
	if srv == nil || fmt.Sprint(nested(t, srv, "spec", "replicas")) != "2" {
		t.Errorf("server replicas not updated to 2")
	}
	if sts := after["StatefulSet/"+ns+"/patrol-controller"]; sts != nil {
		if vct := nested(t, sts, "spec", "volumeClaimTemplates"); vct != nil {
			t.Errorf("controller StatefulSet has volumeClaimTemplates (immutable on upgrade): %v", vct)
		}
		c := nested(t, sts, "spec", "template", "spec", "containers").([]any)[0].(map[string]any)
		if mem := c["resources"].(map[string]any)["limits"].(map[string]any)["memory"]; mem != "3Gi" {
			t.Errorf("controller memory limit = %v, want 3Gi", mem)
		}
	}
	var notif []object
	for _, o := range namespaced {
		if strings.Contains(o.u.GetName(), "notifications") {
			notif = append(notif, o)
		}
	}
	if len(notif) == 0 {
		t.Fatal("no notifications controller objects rendered")
	}
	presence(ctx, t, k, notif, ns, false)

	// 4. Rollback to revision 1.
	h.run(false, "rollback", release, "1", "-n", ns, "--no-hooks", "--timeout", "3m")
	if st, rev := h.status(release, ns); st != "deployed" || rev != 4 {
		t.Fatalf("after rollback: status %s revision %d", st, rev)
	}
	rolled := snapshot()
	presence(ctx, t, k, notif, ns, true)
	for id, got := range rolled {
		if got.GetKind() == "Deployment" || got.GetKind() == "StatefulSet" {
			if _, ok := got.GetLabels()["team"]; ok {
				t.Errorf("%s: label team survived the rollback", id)
			}
			for _, c := range nested(t, got, "spec", "template", "spec", "containers").([]any) {
				img := c.(map[string]any)["image"].(string)
				if strings.HasPrefix(img, "patrol-cd/patrol-cd") && !strings.HasSuffix(img, ":0.1.0") {
					t.Errorf("%s: image %s not rolled back", id, img)
				}
			}
		}
	}
	if fmt.Sprint(nested(t, rolled["Deployment/"+ns+"/patrol-server"], "spec", "replicas")) != "1" {
		t.Errorf("server replicas not rolled back to 1")
	}
	checkKeys("rollback")

	// 5. A second cluster-wide release in another namespace cannot take over the CRDs and the
	// admission policy (cluster-wide singletons owned by the first release).
	out = h.run(true, "install", "other", chartDir(), "-f", defaults, "-n", "other", "--create-namespace", "--no-hooks", "--timeout", "3m")
	if !strings.Contains(out, "invalid ownership metadata") && !strings.Contains(out, "cannot be imported") {
		t.Errorf("second cluster-wide release failed for another reason:\n%s", out)
	}
	t.Logf("second cluster-wide release refused as expected: %s", lastLine(out))
	if r := runCmd(t, time.Minute, h.env, h.bin, "status", "other", "-n", "other"); r.err == nil {
		h.run(false, "uninstall", "other", "-n", "other", "--no-hooks")
	}

	// 6. A namespaced release next to it (CRDs installed by the first release).
	team, err := parseObjects(render(t, "team", "team", []string{filepath.Join(chartDir(), "ci", "namespaced.yaml")}))
	if err != nil {
		t.Fatal(err)
	}
	teamArgs := []string{"install", "team", chartDir(), "-f", filepath.Join(chartDir(), "ci", "namespaced.yaml"), "-n", "team", "--create-namespace", "--no-hooks", "--timeout", "3m"}
	// rbac.managedNamespaces (team-a) must exist before the install: the chart creates Roles
	// there (finding: not stated in the values documentation or the installation guide).
	if out := h.run(true, teamArgs...); !strings.Contains(out, `namespaces "team-a" not found`) {
		t.Errorf("namespaced release without team-a failed for another reason:\n%s", out)
	}
	h.run(false, "uninstall", "team", "-n", "team", "--no-hooks")
	if err := k.ensureNamespace(ctx, "team-a", nil); err != nil {
		t.Fatal(err)
	}
	h.run(false, teamArgs...)
	t.Logf("namespaced release: %d/%d objects present", presence(ctx, t, k, team, "team", true), len(team))
	h.run(false, "uninstall", "team", "-n", "team", "--no-hooks")
	presence(ctx, t, k, team, "team", false)

	// The admin changes the password (as `patrol account update-password` does: a new hash in
	// patrol-secret); the initial password Secret goes stale (bootstrap.md).
	changed := "changed-" + randomHex(t, 8)
	setAdminPassword(ctx, t, k, ns, changed)
	if err := adminLogin(ctx, t, k, ns, changed); err != nil {
		t.Fatalf("the changed admin password does not log in: %v", err)
	}
	if err := adminLogin(ctx, t, k, ns, firstPassword); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("initial password after the password change: login error %v, want %v", err, auth.ErrInvalidCredentials)
	}

	// 7. Uninstall: everything goes except the kept CRDs and admission policy.
	out = h.run(false, "uninstall", release, "-n", ns, "--no-hooks", "--timeout", "3m")
	if !strings.Contains(out, "kept due to the resource policy") {
		t.Errorf("helm uninstall did not report kept resources:\n%s", out)
	}
	presence(ctx, t, k, namespaced, ns, false)
	presence(ctx, t, k, clusterScoped, ns, false)
	presence(ctx, t, k, crds, ns, true)
	// Runtime Secrets after uninstall: the ones only the bootstrap creates stay (bootstrap.md);
	// patrol-secret is rendered by the chart (configs.secret.create) and goes with the release.
	left := runtimeSecrets(ctx, t, k, ns)
	for _, name := range []string{server.InternalTokenSecretName + "/token", server.InitialAdminSecretName + "/password"} {
		if left[name] == "" {
			t.Errorf("uninstall removed %s (created at runtime, documented to survive helm uninstall)", name)
		}
	}
	_, secretErr := k.kube.CoreV1().Secrets(ns).Get(ctx, settings.DefaultSecretName, metav1.GetOptions{})
	t.Logf("after uninstall: %s exists=%v (Helm-managed; server.secretkey and admin.password go with it)", settings.DefaultSecretName, secretErr == nil)

	// 8. Re-install after uninstall (the kept CRDs still carry this release's ownership).
	h.run(false, append([]string{"install", release, chartDir(), "-f", defaults}, common...)...)
	if st, rev := h.status(release, ns); st != "deployed" || rev != 1 {
		t.Fatalf("after re-install: status %s revision %d", st, rev)
	}
	t.Logf("re-install: %d/%d rendered objects present", presence(ctx, t, k, objs, ns, true), len(objs))
	// The re-install rendered patrol-secret again without admin.password, so the kept initial
	// admin Secret predates it (creationTimestamps, second granularity; steps 1 to 8 span many
	// seconds). The bootstrap must replace it with a new password instead of re-hashing the
	// stale one (finding DV-6, bootstrap.md "Stale initial admin Secret"); the internal token
	// is kept.
	ps, err := k.kube.CoreV1().Secrets(ns).Get(ctx, settings.DefaultSecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !ps.CreationTimestamp.After(firstInitial.CreationTimestamp.Time) {
		t.Fatalf("setup: re-installed %s (%s) is not newer than the kept %s (%s)", settings.DefaultSecretName,
			ps.CreationTimestamp, server.InitialAdminSecretName, firstInitial.CreationTimestamp)
	}
	bootstrapRun(t, k, ns)
	again := runtimeSecrets(ctx, t, k, ns)
	if name := server.InternalTokenSecretName + "/" + server.InternalTokenSecretKey; again[name] == "" || again[name] != keys[name] {
		t.Errorf("re-install: %s changed or missing (the kept internal token must be reused)", name)
	}
	if name := server.InitialAdminSecretName + "/" + server.InitialAdminSecretKey; again[name] == "" || again[name] == keys[name] {
		t.Errorf("re-install: %s was not replaced (the stale initial admin password was reused, DV-6)", name)
	}
	if again[settings.DefaultSecretName+"/"+settings.SecretKeyAdminPassword] == "" || again[settings.DefaultSecretName+"/"+settings.SecretKeyServerSecretKey] == "" {
		t.Errorf("re-install: bootstrap did not restore patrol-secret keys")
	}
	ini, err := k.kube.CoreV1().Secrets(ns).Get(ctx, server.InitialAdminSecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if ini.UID == firstInitial.UID {
		t.Errorf("re-install: %s still has the first install's UID %s (not re-created)", server.InitialAdminSecretName, ini.UID)
	}
	if err := adminLogin(ctx, t, k, ns, string(ini.Data[server.InitialAdminSecretKey])); err != nil {
		t.Errorf("re-install: the new initial admin password does not log in: %v", err)
	}
	for what, password := range map[string]string{
		"stale initial password":                firstPassword,
		"password changed before the uninstall": changed,
	} {
		if err := adminLogin(ctx, t, k, ns, password); !errors.Is(err, auth.ErrInvalidCredentials) {
			t.Errorf("re-install: %s: login error %v, want %v", what, err, auth.ErrInvalidCredentials)
		}
	}
	if !t.Failed() {
		t.Logf("after uninstall + re-install: internal token kept; new initial admin password (new Secret UID) logs in; "+
			"stale initial and changed passwords rejected; server.secretkey regenerated=%v",
			again[settings.DefaultSecretName+"/"+settings.SecretKeyServerSecretKey] != keys[settings.DefaultSecretName+"/"+settings.SecretKeyServerSecretKey])
	}
	h.run(false, "uninstall", release, "-n", ns, "--no-hooks")
}

// adminLogin authenticates the admin account with the server's login path (internal/auth)
// against the live patrol-cm and patrol-secret. The password is a TEST-ONLY value and is never
// logged.
func adminLogin(ctx context.Context, t *testing.T, k *kubeEnv, ns, password string) error {
	t.Helper()
	cm, err := k.kube.CoreV1().ConfigMaps(ns).Get(ctx, settings.DefaultConfigMapName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	sec, err := k.kube.CoreV1().Secrets(ns).Get(ctx, settings.DefaultSecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	in := settings.Input{Namespace: ns, ConfigMaps: []*corev1.ConfigMap{cm}, Secrets: []*corev1.Secret{sec}}
	authn, err := auth.NewAuthenticator(settings.Parse(in).Accounts(), auth.LoginConfig{},
		auth.WithLoginLogger(slog.New(slog.DiscardHandler)))
	if err != nil {
		t.Fatal(err)
	}
	_, err = authn.Authenticate(ctx, auth.AdminUsername, password)
	return err
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}

// bootstrapRun runs `patrolcd bootstrap` (the init container's command) with the admin
// kubeconfig.
func bootstrapRun(t *testing.T, k *kubeEnv, ns string) {
	t.Helper()
	bin := patrolcdBinary(t)
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "POD_NAMESPACE=" + ns}
	r := runCmd(t, 2*time.Minute, env, bin, "bootstrap", "--kubeconfig", k.kubeconfig, "--timeout=60s")
	if r.err != nil {
		t.Fatalf("patrolcd bootstrap: %v\n%s", r.err, r.combined())
	}
}

// runtimeSecrets returns digests of the generated Secret material (never the values).
func runtimeSecrets(ctx context.Context, t *testing.T, k *kubeEnv, ns string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for name, keys := range map[string][]string{
		settings.DefaultSecretName:     {settings.SecretKeyServerSecretKey, settings.SecretKeyAdminPassword, settings.SecretKeyAdminPasswordMtime},
		server.InternalTokenSecretName: {server.InternalTokenSecretKey},
		server.InitialAdminSecretName:  {server.InitialAdminSecretKey},
	} {
		s, err := k.kube.CoreV1().Secrets(ns).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, key := range keys {
			if v := s.Data[key]; len(v) > 0 {
				sum := sha256.Sum256(v)
				out[name+"/"+key] = hex.EncodeToString(sum[:8])
			}
		}
	}
	return out
}

// setAdminPassword stores a new admin password hash in patrol-secret as `patrol account
// update-password` does (TEST-ONLY password; minimum bcrypt cost to keep the test fast).
func setAdminPassword(ctx context.Context, t *testing.T, k *kubeEnv, ns, password string) {
	t.Helper()
	h, err := auth.HashPasswordWithCost(password, bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	kv, err := auth.SecretWithPassword(nil, auth.AdminUsername, h, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	s, err := k.kube.CoreV1().Secrets(ns).Get(ctx, settings.DefaultSecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for key, v := range kv {
		s.Data[key] = []byte(v)
	}
	if _, err := k.kube.CoreV1().Secrets(ns).Update(ctx, s, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}
