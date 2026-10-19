package deploy_test

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	autoscalingv1 "k8s.io/api/autoscaling/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/juliosaraiva/patrol-cd/internal/settings"
)

// Field ownership across upgrades. Helm 4 and `kubectl apply --server-side` own every field
// they apply: a field the product rewrites at runtime (trust ConfigMaps written by the
// certificate and GnuPG key APIs, the controller's replica count changed with `kubectl scale`)
// then makes the next `helm upgrade` fail with a conflict, or is silently reverted by a static
// re-apply with --force-conflicts. These tests pin what the chart and the static manifests
// own, and the documented procedures (docs/guides/installation.md, manifests.md).

// TEST-ONLY trust material: syntactically shaped placeholders, not real keys or certificates
// (the API server does not parse ConfigMap data).
const (
	apiHostLine    = "git.example.test ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIPlaceholderTestOnlyNotARealHostKey00000"
	valuesHostLine = "values.example.test ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIPlaceholderTestOnlyValuesManaged0000"
	githubEd25519  = "github.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl"
	apiTLSHost     = "git.example.test"
	apiTLSPEM      = "-----BEGIN CERTIFICATE-----\nTEST-ONLY-PLACEHOLDER\n-----END CERTIFICATE-----\n"
	apiGPGKeyID    = "0123456789ABCDEF"
	apiGPGKey      = "-----BEGIN PGP PUBLIC KEY BLOCK-----\nTEST-ONLY-PLACEHOLDER\n-----END PGP PUBLIC KEY BLOCK-----\n"
	// apiFieldManager stands for patrol-server: the certificate and GnuPG key services
	// (internal/apiserver/services/repositories) write the trust ConfigMaps with Update.
	apiFieldManager = "patrol-server"
)

// configMapData returns a ConfigMap's data (fails when the ConfigMap is missing).
func configMapData(ctx context.Context, t *testing.T, k *kubeEnv, ns, name string) map[string]string {
	t.Helper()
	cm, err := k.kube.CoreV1().ConfigMaps(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("ConfigMap %s/%s: %v", ns, name, err)
	}
	return cm.Data
}

// apiUpdateConfigMap changes a ConfigMap the way patrol-server's APIs do: read, modify, full
// Update carrying the resourceVersion read.
func apiUpdateConfigMap(ctx context.Context, t *testing.T, k *kubeEnv, ns, name string, mutate func(map[string]string)) {
	t.Helper()
	cm, err := k.kube.CoreV1().ConfigMaps(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("ConfigMap %s/%s: %v", ns, name, err)
	}
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	mutate(cm.Data)
	if _, err := k.kube.CoreV1().ConfigMaps(ns).Update(ctx, cm, metav1.UpdateOptions{FieldManager: apiFieldManager}); err != nil {
		t.Fatalf("update ConfigMap %s/%s as %s: %v", ns, name, apiFieldManager, err)
	}
}

// apiTrustChanges makes the changes `patrol cert add-ssh`, `patrol cert rm`, `patrol cert
// add-tls` and `patrol gpg add` make: a known host added, the Bitbucket hosts removed, a TLS
// certificate and a GnuPG key added.
func apiTrustChanges(ctx context.Context, t *testing.T, k *kubeEnv, ns string) {
	t.Helper()
	apiUpdateConfigMap(ctx, t, k, ns, settings.DefaultSSHKnownHostsConfigMapName, func(d map[string]string) {
		var keep []string
		for _, line := range strings.Split(strings.TrimRight(d[settings.SSHKnownHostsKey], "\n"), "\n") {
			if !strings.HasPrefix(line, "bitbucket.org ") {
				keep = append(keep, line)
			}
		}
		d[settings.SSHKnownHostsKey] = strings.Join(append(keep, apiHostLine), "\n") + "\n"
	})
	apiUpdateConfigMap(ctx, t, k, ns, settings.DefaultTLSCertsConfigMapName, func(d map[string]string) { d[apiTLSHost] = apiTLSPEM })
	apiUpdateConfigMap(ctx, t, k, ns, settings.DefaultGPGKeysConfigMapName, func(d map[string]string) { d[apiGPGKeyID] = apiGPGKey })
}

// checkAPITrust reports API changes that step reverted.
func checkAPITrust(ctx context.Context, t *testing.T, k *kubeEnv, ns, step string) {
	t.Helper()
	hosts := configMapData(ctx, t, k, ns, settings.DefaultSSHKnownHostsConfigMapName)[settings.SSHKnownHostsKey]
	switch {
	case !strings.Contains(hosts, apiHostLine):
		t.Errorf("%s reverted the known host added through the API", step)
	case strings.Contains(hosts, "bitbucket.org "):
		t.Errorf("%s restored the known hosts removed through the API", step)
	case !strings.Contains(hosts, githubEd25519):
		t.Errorf("%s removed the default GitHub host key", step)
	default:
		t.Logf("%s kept the API-managed known hosts", step)
	}
	if got := configMapData(ctx, t, k, ns, settings.DefaultTLSCertsConfigMapName)[apiTLSHost]; got != apiTLSPEM {
		t.Errorf("%s reverted the TLS certificate added through the API (got %q)", step, got)
	}
	if got := configMapData(ctx, t, k, ns, settings.DefaultGPGKeysConfigMapName)[apiGPGKeyID]; got != apiGPGKey {
		t.Errorf("%s reverted the GnuPG key added through the API (got %q)", step, got)
	}
}

// fieldOwners returns the field managers (manager/operation) owning data.<key> of a ConfigMap.
func fieldOwners(ctx context.Context, t *testing.T, k *kubeEnv, ns, name, key string) []string {
	t.Helper()
	cm, err := k.kube.CoreV1().ConfigMaps(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("ConfigMap %s/%s: %v", ns, name, err)
	}
	var out []string
	for _, mf := range cm.ManagedFields {
		if mf.FieldsV1 != nil && strings.Contains(mf.FieldsV1.GetRawString(), `"f:`+key+`"`) {
			out = append(out, mf.Manager+"/"+string(mf.Operation))
		}
	}
	return out
}

// requireDefaultKnownHosts fails unless the known hosts hold the providers' default keys.
func requireDefaultKnownHosts(ctx context.Context, t *testing.T, k *kubeEnv, ns, when string) {
	t.Helper()
	hosts := configMapData(ctx, t, k, ns, settings.DefaultSSHKnownHostsConfigMapName)[settings.SSHKnownHostsKey]
	for _, want := range []string{githubEd25519, "gitlab.com ssh-ed25519 ", "bitbucket.org ssh-ed25519 ", "ssh.dev.azure.com ssh-rsa "} {
		if !strings.Contains(hosts, want) {
			t.Fatalf("%s: known hosts lack %q:\n%s", when, want, hosts)
		}
	}
}

// TestTrustConfigMapsHelmUpgrade: known hosts, TLS certificates and GnuPG keys managed through
// the API survive helm upgrade, the bootstrap and helm rollback. Known hosts set through
// values are owned by Helm, and taking over API-managed ones needs --force-conflicts once
// (installation.md, "Trust material").
func TestTrustConfigMapsHelmUpgrade(t *testing.T) {
	t.Parallel()
	k := freshEnv(t, envOptions{name: "trust-helm"})
	h := newHelmCLI(t, k)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	const ns, release = "trust", "patrol"
	defaults := filepath.Join(chartDir(), "ci", "default.yaml")
	common := []string{"-n", ns, "--no-hooks", "--timeout", "3m"}

	h.run(false, append([]string{"install", release, chartDir(), "-f", defaults, "--create-namespace"}, common...)...)
	bootstrapRun(t, k, ns)
	requireDefaultKnownHosts(ctx, t, k, ns, "after install and bootstrap")
	apiTrustChanges(ctx, t, k, ns)

	h.run(false, append([]string{"upgrade", release, chartDir(), "-f", defaults, "--set", "server.replicas=2"}, common...)...)
	if st, rev := h.status(release, ns); st != "deployed" || rev != 2 {
		t.Fatalf("after upgrade: status %s revision %d", st, rev)
	}
	checkAPITrust(ctx, t, k, ns, "helm upgrade")
	bootstrapRun(t, k, ns)
	checkAPITrust(ctx, t, k, ns, "a bootstrap re-run")
	h.run(false, "rollback", release, "1", "-n", ns, "--no-hooks", "--timeout", "3m")
	if st, _ := h.status(release, ns); st != "deployed" {
		t.Fatalf("after rollback: status %s", st)
	}
	checkAPITrust(ctx, t, k, ns, "helm rollback")

	// Values-managed known hosts: Helm owns ssh_known_hosts from then on. The API owned it,
	// so the switch conflicts until --force-conflicts hands it over (documented).
	file := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(file, []byte(valuesHostLine+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	valuesArgs := append([]string{"upgrade", release, chartDir(), "-f", defaults, "--set-file", "configs.sshKnownHosts=" + file}, common...)
	out := h.run(true, valuesArgs...)
	if !strings.Contains(out, "conflict") || !strings.Contains(out, ".data.ssh_known_hosts") {
		t.Errorf("switching to values-managed known hosts failed for another reason:\n%s", out)
	}
	t.Logf("owners of data.ssh_known_hosts before the switch: %v", fieldOwners(ctx, t, k, ns, settings.DefaultSSHKnownHostsConfigMapName, settings.SSHKnownHostsKey))
	h.run(false, append(valuesArgs, "--force-conflicts")...)
	t.Logf("owners after upgrade --force-conflicts: %v", fieldOwners(ctx, t, k, ns, settings.DefaultSSHKnownHostsConfigMapName, settings.SSHKnownHostsKey))
	if got := configMapData(ctx, t, k, ns, settings.DefaultSSHKnownHostsConfigMapName)[settings.SSHKnownHostsKey]; got != valuesHostLine+"\n" {
		t.Errorf("values-managed known hosts = %q, want the values file", got)
	}
	bootstrapRun(t, k, ns)
	if got := configMapData(ctx, t, k, ns, settings.DefaultSSHKnownHostsConfigMapName)[settings.SSHKnownHostsKey]; got != valuesHostLine+"\n" {
		t.Errorf("the bootstrap changed values-managed known hosts: %q", got)
	}

	// Back to API-managed: the upgrade drops the key Helm owned, and the bootstrap of the next
	// patrol-server start (its arguments change, so the pods roll) writes the defaults again.
	// --reset-values: with no values given (ci/default.yaml is empty) Helm would reuse the
	// previous release's values, configs.sshKnownHosts included.
	h.run(false, append([]string{"upgrade", release, chartDir(), "-f", defaults, "--reset-values"}, common...)...)
	if _, ok := configMapData(ctx, t, k, ns, settings.DefaultSSHKnownHostsConfigMapName)[settings.SSHKnownHostsKey]; ok {
		t.Errorf("switching back to API-managed known hosts kept the values-managed key (owners %v)",
			fieldOwners(ctx, t, k, ns, settings.DefaultSSHKnownHostsConfigMapName, settings.SSHKnownHostsKey))
	}
	bootstrapRun(t, k, ns)
	requireDefaultKnownHosts(ctx, t, k, ns, "after switching back and a bootstrap run")
	h.run(false, "uninstall", release, "-n", ns, "--no-hooks")
}

// TestTrustConfigMapsStaticReapply: the documented static upgrade (`kubectl apply
// --server-side --force-conflicts -f install.yaml`) keeps known hosts, TLS certificates and
// GnuPG keys managed through the API.
func TestTrustConfigMapsStaticReapply(t *testing.T) {
	t.Parallel()
	requireManifests(t)
	k := freshEnv(t, envOptions{name: "trust-static"})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	objs, err := readObjects(filepath.Join(manifestsDir(), "install.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	rc := k.kube.Discovery().RESTClient()
	apply := func(pass string) {
		for _, o := range objs {
			if err := k.apply(ctx, rc, o, "", applyOptions{fieldManager: "kubectl", force: true}); err != nil {
				t.Fatalf("%s: %v", pass, err)
			}
		}
	}
	apply("apply")
	bootstrapRun(t, k, releaseNS)
	requireDefaultKnownHosts(ctx, t, k, releaseNS, "after apply and bootstrap")
	apiTrustChanges(ctx, t, k, releaseNS)
	apply("re-apply")
	checkAPITrust(ctx, t, k, releaseNS, "static re-apply with --force-conflicts")
	bootstrapRun(t, k, releaseNS)
	checkAPITrust(ctx, t, k, releaseNS, "a bootstrap re-run")
}

// controllerEnv returns the names of the controller container's environment variables.
func controllerEnv(t *testing.T, sts *unstructured.Unstructured) []string {
	t.Helper()
	var names []string
	for _, c := range nested(t, sts, "spec", "template", "spec", "containers").([]any) {
		cm := c.(map[string]any)
		if cm["name"] != "controller" {
			continue
		}
		for _, e := range cm["env"].([]any) {
			names = append(names, e.(map[string]any)["name"].(string))
		}
	}
	return names
}

// TestControllerReplicasHelm: the number of controller shards is changed with
// controller.replicas. With dynamic cluster distribution the change reaches only the
// StatefulSet's spec.replicas (no rolling restart: the replicas read it live); `kubectl scale`
// on a Helm release makes the next upgrade conflict until --force-conflicts reverts it
// (installation.md, "High availability").
func TestControllerReplicasHelm(t *testing.T) {
	t.Parallel()
	defaults := filepath.Join(chartDir(), "ci", "default.yaml")
	const dynamicFlag = "controller.sharding.dynamicClusterDistribution=true"

	// Static distribution needs the count in the environment; dynamic distribution ignores it.
	for _, tc := range []struct {
		extra   []string
		wantEnv bool
	}{
		{extra: []string{"--set", "controller.replicas=2"}, wantEnv: true},
		{extra: []string{"--set", "controller.replicas=2", "--set", dynamicFlag}, wantEnv: false},
	} {
		objs, err := parseObjects(render(t, releaseName, releaseNS, []string{defaults}, tc.extra...))
		if err != nil {
			t.Fatal(err)
		}
		sts := find(objs, "StatefulSet", "patrol-controller")
		if sts == nil {
			t.Fatal("no controller StatefulSet rendered")
		}
		if got := slices.Contains(controllerEnv(t, sts.u), "PATROL_CONTROLLER_REPLICAS"); got != tc.wantEnv {
			t.Errorf("%v: PATROL_CONTROLLER_REPLICAS rendered=%v, want %v", tc.extra, got, tc.wantEnv)
		}
	}

	k := freshEnv(t, envOptions{name: "shards"})
	h := newHelmCLI(t, k)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	const ns, release = "shards", "patrol"
	common := []string{"-n", ns, "--no-hooks", "--timeout", "3m", "-f", defaults, "--set", dynamicFlag}
	stsObj := object{u: &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1", "kind": "StatefulSet", "metadata": map[string]any{"name": "patrol-controller", "namespace": ns},
	}}}
	getSTS := func() *unstructured.Unstructured {
		t.Helper()
		got, err := k.get(ctx, stsObj, ns)
		if err != nil || got == nil {
			t.Fatalf("controller StatefulSet: %v", err)
		}
		return got
	}
	replicasOf := func(u *unstructured.Unstructured) int64 {
		r, _, _ := unstructured.NestedInt64(u.Object, "spec", "replicas")
		return r
	}

	h.run(false, append([]string{"install", release, chartDir(), "--create-namespace", "--set", "controller.replicas=2"}, common...)...)
	before := getSTS()
	h.run(false, append([]string{"upgrade", release, chartDir(), "--set", "controller.replicas=3"}, common...)...)
	after := getSTS()
	if replicasOf(after) != 3 {
		t.Errorf("controller.replicas=3 upgrade: spec.replicas = %d", replicasOf(after))
	}
	if !reflect.DeepEqual(nested(t, before, "spec", "template"), nested(t, after, "spec", "template")) {
		t.Errorf("changing controller.replicas changed the pod template (rolling restart of every shard) although dynamic cluster distribution reads the count live")
	}

	// kubectl scale (the scale subresource) on a Helm release.
	if _, err := k.kube.AppsV1().StatefulSets(ns).UpdateScale(ctx, "patrol-controller", &autoscalingv1.Scale{
		ObjectMeta: metav1.ObjectMeta{Name: "patrol-controller", Namespace: ns},
		Spec:       autoscalingv1.ScaleSpec{Replicas: 4},
	}, metav1.UpdateOptions{FieldManager: "kubectl"}); err != nil {
		t.Fatal(err)
	}
	upgrade := append([]string{"upgrade", release, chartDir(), "--set", "controller.replicas=3"}, common...)
	out := h.run(true, upgrade...)
	if !strings.Contains(out, "conflict") || !strings.Contains(out, ".spec.replicas") {
		t.Errorf("upgrade after kubectl scale failed for another reason:\n%s", out)
	}
	h.run(false, append(upgrade, "--force-conflicts")...)
	if r := replicasOf(getSTS()); r != 3 {
		t.Errorf("after upgrade --force-conflicts: spec.replicas = %d, want controller.replicas (3)", r)
	}
	h.run(false, "uninstall", release, "-n", ns, "--no-hooks")
}
