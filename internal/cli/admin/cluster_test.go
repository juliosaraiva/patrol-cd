package admin

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/yaml"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/controller/sharding"
)

// createCluster creates a Cluster with a status (connection, apps and resources counts).
func createCluster(t *testing.T, ctx contextT, ns, name, server string, shard *int64, apps, resources int64) *unstructured.Unstructured {
	t.Helper()
	spec := map[string]any{"server": server, "name": name}
	if shard != nil {
		spec["shard"] = *shard
	}
	c := createObject(t, ctx, ns, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": currentGroup + "/v1alpha1", "kind": "Cluster", "metadata": map[string]any{"name": name}, "spec": spec,
	}})
	c.Object["status"] = map[string]any{
		"connectionState": map[string]any{"status": "Successful"},
		"info":            map[string]any{"applicationsCount": apps, "cacheInfo": map[string]any{"resourcesCount": resources}},
	}
	updated, err := envDyn.Resource(kindCluster.gvr(currentGroup)).Namespace(ns).UpdateStatus(ctx, c, metav1.UpdateOptions{})
	require.NoError(t, err)
	return updated
}

func createApp(t *testing.T, ctx contextT, ns, name string, dest map[string]any) {
	t.Helper()
	createObject(t, ctx, ns, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": currentGroup + "/v1alpha1", "kind": "Application", "metadata": map[string]any{"name": name},
		"spec": map[string]any{"project": "default", "source": map[string]any{"repoURL": "https://git.example.com/x.git", "path": "."}, "destination": dest},
	}})
}

func createControllerPod(t *testing.T, ctx contextT, ns, name string) {
	t.Helper()
	_, err := envKube.CoreV1().Pods(ns).Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{v1alpha1.LabelKeyAppName: sharding.DefaultApplicationControllerName}},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "example.invalid/controller:dev"}}},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
}

func disableInCluster(t *testing.T, ctx contextT, ns string) {
	t.Helper()
	createObject(t, ctx, ns, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "patrol-cm", "labels": map[string]any{"app.kubernetes.io/part-of": "patrol-cd"}},
		"data": map[string]any{"cluster.inClusterEnabled": "false"},
	}})
}

func TestClusterStatsAndShards(t *testing.T) {
	ctx := requireEnv(t)
	ns := newNamespace(t, ctx, "stats")
	tio := newTestIO(ns)

	// No controller pods and no --replicas: shards prints nothing (upstream).
	require.NoError(t, tio.run(ctx, "admin", "cluster", "shards"))
	assert.Empty(t, tio.out.String())

	disableInCluster(t, ctx, ns)
	pin := int64(1)
	var created []*unstructured.Unstructured
	for i, res := range []int64{100, 300, 50, 50} {
		var shard *int64
		if i == 3 {
			shard = &pin
		}
		created = append(created, createCluster(t, ctx, ns, fmt.Sprintf("c%d", i+1), fmt.Sprintf("https://c%d.example.com", i+1), shard, int64(i), res))
	}
	createApp(t, ctx, ns, "a1", map[string]any{"server": "https://c1.example.com", "namespace": "team-a"})
	createApp(t, ctx, ns, "a2", map[string]any{"server": "https://c1.example.com", "namespace": "team-b"})
	createApp(t, ctx, ns, "a3", map[string]any{"name": "c2", "namespace": "web"})
	createControllerPod(t, ctx, ns, "patrol-controller-0")
	createControllerPod(t, ctx, ns, "patrol-controller-1")

	// Expected shards: the legacy algorithm over the Cluster UIDs, pins win.
	var infos []sharding.ClusterInfo
	for _, c := range created {
		cl := &v1alpha1.Cluster{}
		require.NoError(t, runtimeFromUnstructured(c, cl))
		infos = append(infos, sharding.ClusterInfoFrom(cl))
	}
	want := sharding.Distribute(sharding.AlgorithmLegacy, infos, nil, 2, nil)
	assert.Equal(t, 1, want["https://c4.example.com"])

	require.NoError(t, tio.run(ctx, "admin", "cluster", "stats"))
	out := tio.out.String()
	lines := strings.Split(strings.TrimSpace(out), "\n")
	require.Len(t, lines, 5, out)
	assert.Equal(t, []string{"SERVER", "SHARD", "CONNECTION", "NAMESPACES", "COUNT", "APPS", "COUNT", "RESOURCES", "COUNT"}, strings.Fields(lines[0]))
	for i, line := range lines[1:] {
		server := fmt.Sprintf("https://c%d.example.com", i+1)
		fields := strings.Fields(line)
		require.Len(t, fields, 6, line)
		assert.Equal(t, server, fields[0])
		assert.Equal(t, fmt.Sprint(want[server]), fields[1], server)
		assert.Equal(t, "Successful", fields[2])
	}
	assert.Equal(t, []string{"https://c1.example.com", fmt.Sprint(want["https://c1.example.com"]), "Successful", "2", "0", "100"}, strings.Fields(lines[1]))
	assert.Equal(t, "1", strings.Fields(lines[2])[3], "c2 is resolved by destination name")

	require.NoError(t, tio.run(ctx, "admin", "cluster", "stats", "--shard", "1"))
	for _, line := range strings.Split(strings.TrimSpace(tio.out.String()), "\n")[1:] {
		assert.Equal(t, "1", strings.Fields(line)[1], "--shard filters rows")
	}

	require.NoError(t, tio.run(ctx, "admin", "cluster", "shards", "--port-forward-redis=false"))
	out = tio.out.String()
	var per [2]int64
	for i, res := range []int64{100, 300, 50, 50} {
		per[want[fmt.Sprintf("https://c%d.example.com", i+1)]] += res
	}
	assert.Contains(t, out, "SHARD  RESOURCES COUNT\n")
	assert.Contains(t, out, fmt.Sprintf("0      %d (%.0f%%)\n", per[0], float64(per[0])/250*100))
	assert.Contains(t, out, fmt.Sprintf("1      %d (%.0f%%)\n", per[1], float64(per[1])/250*100))
	assert.Contains(t, tio.errOut.String(), "Patrol CD has no Redis", "Redis flags are accepted and reported as ignored")

	// round-robin: rank of the UID among sorted UIDs.
	require.NoError(t, tio.run(ctx, "admin", "cluster", "stats", "--replicas", "3", "--sharding-method", "round-robin"))
	want3 := sharding.Distribute(sharding.AlgorithmRoundRobin, infos, nil, 3, nil)
	for _, line := range strings.Split(strings.TrimSpace(tio.out.String()), "\n")[1:] {
		f := strings.Fields(line)
		assert.Equal(t, fmt.Sprint(want3[f[0]]), f[1], f[0])
	}

	// An unresolvable destination aborts, as upstream.
	createApp(t, ctx, ns, "broken", map[string]any{"name": "nope", "namespace": "x"})
	err := tio.run(ctx, "admin", "cluster", "stats")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "error validating application destination: there are no clusters with this name: nope")
}

func TestClusterNamespacesAndNamespacedMode(t *testing.T) {
	ctx := requireEnv(t)
	ns := newNamespace(t, ctx, "nsmode")
	tio := newTestIO(ns)
	createCluster(t, ctx, ns, "c1", "https://c1.example.com", nil, 0, 0)
	createCluster(t, ctx, ns, "c2", "https://c2.example.com", nil, 0, 0)
	for i, n := range []string{"team-a", "b", "team-ccc", "dd", "eeeee"} {
		createApp(t, ctx, ns, fmt.Sprintf("app%d", i), map[string]any{"server": "https://c1.example.com", "namespace": n})
	}
	createApp(t, ctx, ns, "other", map[string]any{"server": "https://c2.example.com", "namespace": "web"})

	require.NoError(t, tio.run(ctx, "admin", "cluster", "namespaces"))
	out := tio.out.String()
	rows := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n")[1:] {
		f := strings.Fields(line)
		rows[f[0]] = strings.Join(f[1:], " ")
	}
	assert.Equal(t, "b,dd,eeeee,team-a (total 5)", rows["https://c1.example.com"], "shortest first, first 4")
	assert.Equal(t, "web", rows["https://c2.example.com"])
	assert.Contains(t, out, "https://kubernetes.default.svc", "the synthetic in-cluster cluster is listed")

	// Dry run by default: nothing changes.
	require.NoError(t, tio.run(ctx, "admin", "cluster", "namespaces", "enable-namespaced-mode", "https://c2*", "--max-namespace-count", "5"))
	assert.Equal(t, "Setting cluster https://c2.example.com namespaces to [web]...done (dry run)\n", tio.out.String())
	var c2 v1alpha1.Cluster
	getCluster(t, ctx, ns, "c2", &c2)
	assert.Empty(t, c2.Spec.Namespaces)

	// c1 uses more namespaces than allowed.
	require.NoError(t, tio.run(ctx, "admin", "cluster", "namespaces", "enable-namespaced-mode", "*", "--max-namespace-count", "1", "--dry-run=false", "--cluster-resources"))
	assert.Equal(t, "Setting cluster https://c2.example.com namespaces to [web]...done\n", tio.out.String())
	getCluster(t, ctx, ns, "c2", &c2)
	assert.Equal(t, []string{"web"}, c2.Spec.Namespaces)
	assert.True(t, c2.Spec.ClusterResources)
	var c1 v1alpha1.Cluster
	getCluster(t, ctx, ns, "c1", &c1)
	assert.Empty(t, c1.Spec.Namespaces)

	require.NoError(t, tio.run(ctx, "admin", "cluster", "namespaces", "disable-namespaced-mode", "https://c*"))
	assert.Equal(t, "Disabling namespaced mode for cluster https://c2.example.com...done (dry run)\n", tio.out.String())
	require.NoError(t, tio.run(ctx, "admin", "cluster", "namespaces", "disable-namespaced-mode", "https://c*", "--dry-run=false"))
	getCluster(t, ctx, ns, "c2", &c2)
	assert.Empty(t, c2.Spec.Namespaces)
}

func getCluster(t *testing.T, ctx contextT, ns, name string, into *v1alpha1.Cluster) {
	t.Helper()
	u, err := envDyn.Resource(kindCluster.gvr(currentGroup)).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	require.NoError(t, err)
	*into = v1alpha1.Cluster{}
	require.NoError(t, runtimeFromUnstructured(u, into))
}

// envKubeconfig writes a kubeconfig for the envtest API server.
func envKubeconfig(t *testing.T, contextName string) string {
	t.Helper()
	cfg := clientcmdapi.NewConfig()
	cl := clientcmdapi.NewCluster()
	cl.Server = envCfg.Host
	cl.CertificateAuthorityData = envCfg.CAData
	ai := clientcmdapi.NewAuthInfo()
	ai.ClientCertificateData = envCfg.CertData
	ai.ClientKeyData = envCfg.KeyData
	ai.Token = envCfg.BearerToken
	cfg.Clusters["env"] = cl
	cfg.AuthInfos["env"] = ai
	cfg.Contexts[contextName] = &clientcmdapi.Context{Cluster: "env", AuthInfo: "env"}
	cfg.CurrentContext = contextName
	path := filepath.Join(t.TempDir(), "kubeconfig")
	require.NoError(t, clientcmd.WriteToFile(*cfg, path))
	return path
}

func TestClusterKubeconfig(t *testing.T) {
	ctx := requireEnv(t)
	ns := newNamespace(t, ctx, "kubeconfig")
	tio := newTestIO(ns)
	// A Cluster registering the envtest API server itself, with client certificate
	// credentials in its Secret.
	createObject(t, ctx, ns, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Secret", "metadata": map[string]any{"name": "self-creds"},
		"data": map[string]any{"caData": b64(envCfg.CAData), "certData": b64(envCfg.CertData), "keyData": b64(envCfg.KeyData)},
	}})
	createObject(t, ctx, ns, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": currentGroup + "/v1alpha1", "kind": "Cluster", "metadata": map[string]any{"name": "self"},
		"spec": map[string]any{"server": envCfg.Host, "name": "self", "config": map[string]any{"credentialsRef": map[string]any{"name": "self-creds"}}},
	}})

	out := filepath.Join(t.TempDir(), "self.kubeconfig")
	require.NoError(t, tio.run(ctx, "admin", "cluster", "kubeconfig", envCfg.Host, out))
	st, err := os.Stat(out)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), st.Mode().Perm())
	restCfg, err := clientcmd.BuildConfigFromFlags("", out)
	require.NoError(t, err)
	kube, err := kubernetes.NewForConfig(restCfg)
	require.NoError(t, err)
	_, err = kube.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
	require.NoError(t, err, "the generated kubeconfig authenticates against the cluster")
	loaded, err := clientcmd.LoadFromFile(out)
	require.NoError(t, err)
	assert.Equal(t, ns, loaded.Contexts[loaded.CurrentContext].Namespace)

	// Printed: credentials redacted.
	require.NoError(t, tio.run(ctx, "admin", "cluster", "kubeconfig", envCfg.Host))
	printed := tio.out.String()
	assert.Contains(t, printed, "client-key-data: "+b64([]byte(redactedValue)))
	assert.NotContains(t, printed, b64(envCfg.KeyData))
	assert.Contains(t, tio.errOut.String(), "credentials are redacted")

	err = tio.run(ctx, "admin", "cluster", "kubeconfig", "https://unknown.example.com", filepath.Join(t.TempDir(), "x"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestClusterGenerateSpec(t *testing.T) {
	ctx := requireEnv(t)
	kubeconfig := envKubeconfig(t, "envtest")
	tio := newTestIO("default")
	tio.f.RESTConfig = nil // generate-spec reads the kubeconfig only

	require.NoError(t, tio.run(ctx, "admin", "cluster", "generate-spec", "envtest", "--kubeconfig", kubeconfig, "--label", "env=test", "--namespace", "team-a", "--cluster-resources", "--project", "p1"))
	objs, err := splitObjects(tio.out.Bytes())
	require.NoError(t, err)
	require.Len(t, objs, 2)
	cluster, secret := objs[0], objs[1]
	assert.Equal(t, "Cluster", cluster.GetKind())
	assert.Equal(t, currentGroup+"/v1alpha1", cluster.GetAPIVersion())
	server, _, _ := unstructured.NestedString(cluster.Object, "spec", "server")
	assert.Equal(t, envCfg.Host, server)
	name, _, _ := unstructured.NestedString(cluster.Object, "spec", "name")
	assert.Equal(t, "envtest", name)
	assert.Equal(t, map[string]string{"env": "test"}, cluster.GetLabels())
	nsList, _, _ := unstructured.NestedStringSlice(cluster.Object, "spec", "namespaces")
	assert.Equal(t, []string{"team-a"}, nsList)
	ref, _, _ := unstructured.NestedString(cluster.Object, "spec", "config", "credentialsRef", "name")
	assert.Equal(t, secret.GetName(), ref)
	assert.Equal(t, "cluster", secret.GetLabels()[v1alpha1.LabelKeySecretType])
	sd, _, _ := unstructured.NestedStringMap(secret.Object, "stringData")
	assert.Equal(t, placeholderBearerToken, sd["bearerToken"], "placeholder token is not a secret")
	assert.Equal(t, redactedValue, sd["keyData"])
	assert.Equal(t, string(envCfg.CertData), sd["certData"])
	assert.Contains(t, tio.errOut.String(), "credentials are redacted")

	file := filepath.Join(t.TempDir(), "cluster.yaml")
	require.NoError(t, tio.run(ctx, "admin", "cluster", "generate-spec", "envtest", "--kubeconfig", kubeconfig, "--in-cluster", "--bearer-token", "tok-123", "--output-file", file))
	data, err := os.ReadFile(file)
	require.NoError(t, err)
	assert.Contains(t, string(data), "server: https://kubernetes.default.svc")
	assert.Contains(t, string(data), "bearerToken: tok-123")
	assert.Contains(t, string(data), "keyData:")
	assert.NotContains(t, string(data), redactedValue)

	require.NoError(t, tio.run(ctx, "admin", "cluster", "generate-spec", "envtest", "--kubeconfig", kubeconfig, "--aws-cluster-name", "prod", "--aws-role-arn", "arn:aws:iam::1:role/x", "-o", "json"))
	var list map[string]any
	require.NoError(t, yaml.Unmarshal(tio.out.Bytes(), &list))
	assert.Equal(t, "List", list["kind"])
	assert.Contains(t, tio.out.String(), `"clusterName": "prod"`)
	assert.NotContains(t, tio.out.String(), `"bearerToken"`, "AWS authentication needs no bearer token")

	err = tio.run(ctx, "admin", "cluster", "generate-spec", "missing", "--kubeconfig", kubeconfig)
	require.EqualError(t, err, "Context missing does not exist in kubeconfig")
	err = tio.run(ctx, "admin", "cluster", "generate-spec", "envtest", "--kubeconfig", kubeconfig, "--in-cluster", "--cluster-endpoint", "kubeconfig")
	require.EqualError(t, err, "Can only use one of --in-cluster or --cluster-endpoint")
	err = tio.run(ctx, "admin", "cluster", "generate-spec", "--kubeconfig", kubeconfig)
	assert.Equal(t, 1, ExitCode(err))
	assert.Contains(t, err.Error(), "envtest")
}

func TestClusterGenerateSpecBearerToken(t *testing.T) {
	ctx := requireEnv(t)
	kubeconfig := envKubeconfig(t, "envtest")
	tio := newTestIO("default")
	tio.f.RESTConfig = nil
	sa := fmt.Sprintf("patrol-manager-%d", nsSeq.Add(1))
	_, err := envKube.CoreV1().ServiceAccounts("kube-system").Create(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: sa}}, metav1.CreateOptions{})
	require.NoError(t, err)
	const token = "emulated-service-account-token"
	// envtest runs no token controller: emulate it for the token Secret.
	go func() {
		for ctx.Err() == nil {
			sec, err := envKube.CoreV1().Secrets("kube-system").Get(ctx, sa+"-long-lived-token", metav1.GetOptions{})
			if err == nil && len(sec.Data["token"]) == 0 {
				sec.Data = map[string][]byte{"token": []byte(token)}
				_, _ = envKube.CoreV1().Secrets("kube-system").Update(ctx, sec, metav1.UpdateOptions{})
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()
	require.NoError(t, tio.run(ctx, "admin", "cluster", "generate-spec", "envtest", "--kubeconfig", kubeconfig, "--generate-bearer-token", "--service-account", sa))
	assert.NotContains(t, tio.out.String()+tio.errOut.String(), token, "the generated token is redacted on stdout")
	assert.Contains(t, tio.out.String(), "bearerToken: "+redactedValue)

	file := filepath.Join(t.TempDir(), "cluster.yaml")
	require.NoError(t, tio.run(ctx, "admin", "cluster", "generate-spec", "envtest", "--kubeconfig", kubeconfig, "--generate-bearer-token", "--service-account", sa, "--output-file", file))
	data, err := os.ReadFile(file)
	require.NoError(t, err)
	assert.Contains(t, string(data), "bearerToken: "+token, "the existing token Secret is reused")
	st, err := os.Stat(file)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), st.Mode().Perm())
}
