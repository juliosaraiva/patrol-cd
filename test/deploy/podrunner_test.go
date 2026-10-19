package deploy_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/juliosaraiva/patrol-cd/internal/render/cmpwire"
	"github.com/juliosaraiva/patrol-cd/internal/reposerver"
	"github.com/juliosaraiva/patrol-cd/internal/server"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
)

// podRunner is a minimal kubelet for envtest (which has none): it runs the patrolcd
// containers of rendered workloads as local processes of the patrolcd binary, with the
// rendered arguments and environment, the pod's service account identity and emulated
// volumes. Emulation changes are recorded as deviations (see deviationsDoc); everything
// else (flags, environment, Secrets and ConfigMaps read from the API server, init-container
// ordering, probes, SIGTERM handling) is as rendered.
type podRunner struct {
	t      testing.TB
	k      *kubeEnv
	ns     string
	objs   []object
	binary string
	root   string
	pods   []*localPod
	// svcAddr maps Service addresses (name[.ns[.svc[.cluster.local]]]:port) to loopback.
	svcAddr map[string]string
	svcKeys []string
	// saConfig maps service account names to kubeconfig files (TokenRequest tokens).
	saConfig map[string]string
	// extraArgs are appended to a container's arguments (by container name): harness-only
	// changes, each recorded as a deviation by the caller.
	extraArgs map[string][]string
	// skip names containers that are not started (other images, e.g. Dex).
	skipped []string
}

// localPod is one emulated pod.
type localPod struct {
	name   string // pod name: <workload>-0
	owner  string // kind/namespace/name
	kind   string
	wlName string
	tmpl   corev1.PodTemplateSpec
	dir    string
	vols   map[string]string // volume name → host directory
	ports  map[int32]int     // containerPort → local port
	procs  []*localProc
	inits  []*localProc
}

// localProc is one container process.
type localProc struct {
	pod  *localPod
	c    corev1.Container
	pc   parsedContainer
	args []string
	env  []string
	cmd  *exec.Cmd
	log  *syncBuffer
	done chan struct{}
	err  error
	// stopped is set when the harness sent SIGTERM.
	stopped bool
	stopDur time.Duration
}

func (p *localProc) name() string { return p.pod.name + "/" + p.c.Name }

// syncBuffer is a goroutine-safe log buffer.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// newPodRunner prepares pods for the Deployments and StatefulSets of objs (one replica each)
// whose containers run patrolcd, allocates loopback ports and maps the Services.
func newPodRunner(t testing.TB, k *kubeEnv, ns string, objs []object) *podRunner {
	t.Helper()
	r := &podRunner{
		t: t, k: k, ns: ns, objs: objs, binary: patrolcdBinary(t), root: t.TempDir(),
		svcAddr: map[string]string{}, saConfig: map[string]string{}, extraArgs: map[string][]string{},
	}
	for i, pt := range podTemplates(t, objs) {
		parts := strings.Split(pt.owner, "/")
		p := &localPod{
			name: parts[2] + "-0", owner: pt.owner, kind: parts[0], wlName: parts[2], tmpl: pt.tmpl,
			dir: filepath.Join(r.root, fmt.Sprintf("pod%d", i)), vols: map[string]string{}, ports: map[int32]int{},
		}
		hasPatrol := false
		for _, c := range append(slices.Clone(pt.tmpl.Spec.InitContainers), pt.tmpl.Spec.Containers...) {
			if isPatrolcd(c) {
				hasPatrol = true
			}
		}
		if !hasPatrol {
			r.skipped = append(r.skipped, pt.owner)
			continue
		}
		for _, c := range pt.tmpl.Spec.Containers {
			for _, cp := range c.Ports {
				p.ports[cp.ContainerPort] = freePort(t)
			}
		}
		r.pods = append(r.pods, p)
	}
	r.mapServices()
	// Cleanups run in reverse order: kill the processes, then make the pod directories
	// removable (the repo-server makes its root traversable but not listable) before
	// t.TempDir removes them.
	t.Cleanup(func() { makeRemovable(r.root) })
	t.Cleanup(r.killAll)
	return r
}

// makeRemovable restores owner permissions below dir.
func makeRemovable(dir string) {
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			_ = os.Chmod(p, 0o700)
		}
		return nil
	})
}

func freePort(t testing.TB) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

// mapServices maps every rendered Service port to the loopback port of the pod it selects.
func (r *podRunner) mapServices() {
	for _, o := range byKind(r.objs, "Service") {
		var svc corev1.Service
		if err := fromUnstructuredErr(o, &svc); err != nil {
			r.t.Fatal(err)
		}
		sel := labels.SelectorFromSet(svc.Spec.Selector)
		for _, p := range r.pods {
			if strings.Split(p.owner, "/")[1] != svc.Namespace || !sel.Matches(labels.Set(p.tmpl.Labels)) {
				continue
			}
			for _, sp := range svc.Spec.Ports {
				cp := containerPortFor(p.tmpl.Spec, sp)
				local, ok := p.ports[cp]
				if !ok {
					continue
				}
				to := "127.0.0.1:" + strconv.Itoa(local)
				for _, host := range []string{svc.Name, svc.Name + "." + svc.Namespace, svc.Name + "." + svc.Namespace + ".svc", svc.Name + "." + svc.Namespace + ".svc.cluster.local"} {
					r.svcAddr[host+":"+strconv.Itoa(int(sp.Port))] = to
				}
			}
		}
	}
	for key := range r.svcAddr {
		r.svcKeys = append(r.svcKeys, key)
	}
	sort.Slice(r.svcKeys, func(i, j int) bool { return len(r.svcKeys[i]) > len(r.svcKeys[j]) })
}

func containerPortFor(spec corev1.PodSpec, sp corev1.ServicePort) int32 {
	tp := sp.TargetPort
	if tp.Type == intstr.Int && tp.IntVal == 0 {
		return sp.Port
	}
	if tp.Type == intstr.Int {
		return tp.IntVal
	}
	for _, c := range spec.Containers {
		for _, p := range c.Ports {
			if p.Name == tp.StrVal {
				return p.ContainerPort
			}
		}
	}
	return -1
}

// pod returns the emulated pod of a workload.
func (r *podRunner) pod(workload string) *localPod {
	for _, p := range r.pods {
		if p.wlName == workload {
			return p
		}
	}
	return nil
}

// serviceAccountConfigs writes a kubeconfig per service account of pods that mount a token.
func (r *podRunner) serviceAccountConfigs(ctx context.Context) error {
	for _, p := range r.pods {
		if p.tmpl.Spec.AutomountServiceAccountToken == nil || !*p.tmpl.Spec.AutomountServiceAccountToken {
			continue
		}
		sa := p.tmpl.Spec.ServiceAccountName
		if sa == "" {
			sa = "default"
		}
		if _, ok := r.saConfig[sa]; ok {
			continue
		}
		path, _, err := r.k.serviceAccountKubeconfig(ctx, r.root, r.ns, sa)
		if err != nil {
			return err
		}
		r.saConfig[sa] = path
	}
	return nil
}

// errVolumeNotReady means a required Secret or ConfigMap (or key) does not exist yet; the
// kubelet keeps the pod in ContainerCreating and retries.
type errVolumeNotReady struct{ msg string }

func (e errVolumeNotReady) Error() string { return e.msg }

// setupVolumes materializes the pod's volumes from the API server.
func (r *podRunner) setupVolumes(ctx context.Context, p *localPod) error {
	for _, v := range p.tmpl.Spec.Volumes {
		dir := filepath.Join(p.dir, "volumes", v.Name)
		if err := os.RemoveAll(dir); err != nil {
			return err
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		p.vols[v.Name] = dir
		switch {
		case v.EmptyDir != nil:
		case v.Secret != nil:
			if err := r.project(ctx, dir, "Secret", v.Secret.SecretName, v.Secret.Items, v.Secret.Optional); err != nil {
				return err
			}
		case v.ConfigMap != nil:
			if err := r.project(ctx, dir, "ConfigMap", v.ConfigMap.Name, v.ConfigMap.Items, v.ConfigMap.Optional); err != nil {
				return err
			}
		case v.Projected != nil:
			for _, s := range v.Projected.Sources {
				var err error
				switch {
				case s.Secret != nil:
					err = r.project(ctx, dir, "Secret", s.Secret.Name, s.Secret.Items, s.Secret.Optional)
				case s.ConfigMap != nil:
					err = r.project(ctx, dir, "ConfigMap", s.ConfigMap.Name, s.ConfigMap.Items, s.ConfigMap.Optional)
				default:
					err = fmt.Errorf("volume %s: unsupported projected source", v.Name)
				}
				if err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf("volume %s: unsupported volume type", v.Name)
		}
	}
	return nil
}

// project writes the keys of a Secret or ConfigMap into dir (items select and rename keys).
func (r *podRunner) project(ctx context.Context, dir, kind, name string, items []corev1.KeyToPath, optional *bool) error {
	opt := optional != nil && *optional
	data, err := r.objectData(ctx, kind, name)
	if apierrors.IsNotFound(err) {
		if opt {
			return nil
		}
		return errVolumeNotReady{fmt.Sprintf("%s %s/%s not found", kind, r.ns, name)}
	}
	if err != nil {
		return err
	}
	if len(items) == 0 {
		for key := range data {
			items = append(items, corev1.KeyToPath{Key: key, Path: key})
		}
	}
	for _, it := range items {
		val, ok := data[it.Key]
		if !ok {
			if opt {
				continue
			}
			return errVolumeNotReady{fmt.Sprintf("%s %s/%s has no key %q", kind, r.ns, name, it.Key)}
		}
		f := filepath.Join(dir, filepath.FromSlash(it.Path))
		if err := os.MkdirAll(filepath.Dir(f), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(f, val, 0o600); err != nil {
			return err
		}
	}
	return nil
}

func (r *podRunner) objectData(ctx context.Context, kind, name string) (map[string][]byte, error) {
	out := map[string][]byte{}
	switch kind {
	case "Secret":
		s, err := r.k.kube.CoreV1().Secrets(r.ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		for key, v := range s.Data {
			out[key] = v
		}
		for key, v := range s.StringData {
			out[key] = []byte(v)
		}
	case "ConfigMap":
		cm, err := r.k.kube.CoreV1().ConfigMaps(r.ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		for key, v := range cm.Data {
			out[key] = []byte(v)
		}
		for key, v := range cm.BinaryData {
			out[key] = v
		}
	}
	return out, nil
}

// hostPath maps a container path to the emulated volume directory (unchanged when no mount
// covers it).
func (p *localPod) hostPath(c corev1.Container, cpath string) (string, bool) {
	m, rel := mountFor(c, cpath)
	if m == nil {
		return cpath, false
	}
	base := p.vols[m.Name]
	if m.SubPath != "" {
		base = filepath.Join(base, filepath.FromSlash(m.SubPath))
	}
	return filepath.Join(base, filepath.FromSlash(rel)), true
}

// rewrite maps absolute container paths to their volume directories and Service addresses
// to loopback.
func (r *podRunner) rewrite(p *localPod, c corev1.Container, v string) string {
	if strings.HasPrefix(v, "/") {
		v, _ = p.hostPath(c, v)
	}
	for _, key := range r.svcKeys {
		v = strings.ReplaceAll(v, key, r.svcAddr[key])
	}
	return v
}

var varRef = regexp.MustCompile(`\$\(([A-Za-z_][A-Za-z0-9_.-]*)\)`)

// expand resolves $(VAR) references like the kubelet does.
func expand(s string, env map[string]string) string {
	return varRef.ReplaceAllStringFunc(s, func(m string) string {
		if v, ok := env[varRef.FindStringSubmatch(m)[1]]; ok {
			return v
		}
		return m
	})
}

// containerEnv resolves the container's environment as the kubelet would, then applies the
// emulation: paths and Service addresses rewritten, the service account kubeconfig, and the
// parameter values (ConfigMap or compiled defaults) that name mounted paths or Services.
func (r *podRunner) containerEnv(ctx context.Context, p *localPod, pc parsedContainer) ([]string, error) {
	c := pc.c
	env := map[string]string{}
	var order []string
	set := func(k, v string) {
		if _, ok := env[k]; !ok {
			order = append(order, k)
		}
		env[k] = v
	}
	for _, ef := range c.EnvFrom {
		kind, name, optional := "", "", false
		switch {
		case ef.ConfigMapRef != nil:
			kind, name, optional = "ConfigMap", ef.ConfigMapRef.Name, ef.ConfigMapRef.Optional != nil && *ef.ConfigMapRef.Optional
		case ef.SecretRef != nil:
			kind, name, optional = "Secret", ef.SecretRef.Name, ef.SecretRef.Optional != nil && *ef.SecretRef.Optional
		}
		data, err := r.objectData(ctx, kind, name)
		if apierrors.IsNotFound(err) && optional {
			continue
		}
		if err != nil {
			return nil, err
		}
		keys := make([]string, 0, len(data))
		for key := range data {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			set(ef.Prefix+key, string(data[key]))
		}
	}
	for _, e := range c.Env {
		if e.ValueFrom == nil {
			set(e.Name, expand(e.Value, env))
			continue
		}
		vf := e.ValueFrom
		switch {
		case vf.FieldRef != nil:
			switch vf.FieldRef.FieldPath {
			case "metadata.namespace":
				set(e.Name, r.ns)
			case "metadata.name", "spec.nodeName":
				set(e.Name, p.name)
			case "status.podIP", "status.hostIP":
				set(e.Name, "127.0.0.1")
			case "spec.serviceAccountName":
				set(e.Name, p.tmpl.Spec.ServiceAccountName)
			default:
				return nil, fmt.Errorf("%s: unsupported fieldRef %s", e.Name, vf.FieldRef.FieldPath)
			}
		case vf.ResourceFieldRef != nil:
			res := c.Resources.Limits
			q, ok := res[corev1.ResourceName(strings.TrimPrefix(vf.ResourceFieldRef.Resource, "limits."))]
			if !ok {
				continue
			}
			set(e.Name, strconv.FormatInt(q.Value(), 10))
		case vf.ConfigMapKeyRef != nil, vf.SecretKeyRef != nil:
			kind, name, key, optional := "ConfigMap", "", "", false
			if ref := vf.ConfigMapKeyRef; ref != nil {
				name, key, optional = ref.Name, ref.Key, ref.Optional != nil && *ref.Optional
			} else {
				ref := vf.SecretKeyRef
				kind, name, key, optional = "Secret", ref.Name, ref.Key, ref.Optional != nil && *ref.Optional
			}
			data, err := r.objectData(ctx, kind, name)
			if apierrors.IsNotFound(err) && optional {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("%s: %w", e.Name, err)
			}
			v, ok := data[key]
			if !ok {
				if optional {
					continue
				}
				return nil, errVolumeNotReady{fmt.Sprintf("%s %s has no key %s (env %s)", kind, name, key, e.Name)}
			}
			set(e.Name, string(v))
		}
	}
	for _, k := range order {
		env[k] = r.rewrite(p, c, env[k])
	}
	// The pod's identity: in-cluster configuration through a kubeconfig with a TokenRequest
	// token of the service account (also used for the in-cluster destination).
	if cfg, ok := r.saConfig[p.tmpl.Spec.ServiceAccountName]; ok && p.tmpl.Spec.AutomountServiceAccountToken != nil && *p.tmpl.Spec.AutomountServiceAccountToken {
		set("KUBECONFIG", cfg)
		set("PATROL_FAKE_IN_CLUSTER", "true")
	}
	// Parameters configured in patrol-cmd-params-cm (or compiled defaults) that name mounted
	// files or Services: presented as environment variables with the emulated value.
	if pc.spec.settings != "" {
		cm, err := r.objectData(ctx, "ConfigMap", settings.DefaultCmdParamsConfigMapName)
		if err != nil && !apierrors.IsNotFound(err) {
			return nil, err
		}
		for _, spec := range settings.ParamsForComponent(pc.spec.settings) {
			if spec.Env == "" || (spec.Flag != "" && pc.fs.Lookup(spec.Flag) != nil && pc.fs.Changed(spec.Flag)) {
				continue
			}
			if _, ok := env[spec.Env]; ok {
				continue
			}
			raw := spec.Default
			if v, ok := cm[spec.Key]; ok {
				raw = string(v)
			}
			if nv := r.rewrite(p, c, raw); nv != raw {
				set(spec.Env, nv)
			}
		}
	}
	if pc.spec.token && !pc.fs.Changed("internal-token-file") {
		if _, ok := env[server.EnvInternalTokenFile]; !ok {
			if hp, ok := p.hostPath(c, server.DefaultInternalTokenFile); ok {
				set(server.EnvInternalTokenFile, hp)
			}
		}
	}
	if hp, ok := p.hostPath(c, server.DefaultProfilerFile); ok {
		set(server.EnvProfilerFile, hp)
	}
	home := filepath.Join(p.dir, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, err
	}
	tmp, ok := p.hostPath(c, "/tmp")
	if !ok {
		tmp = filepath.Join(p.dir, "tmp-"+c.Name)
		if err := os.MkdirAll(tmp, 0o700); err != nil {
			return nil, err
		}
	}
	out := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "TMPDIR=" + tmp, "LANG=C"}
	for _, k := range order {
		out = append(out, k+"="+env[k])
	}
	return out, nil
}

// containerArgs returns the emulated command line: the rendered arguments with paths and
// Service addresses rewritten, flags for implicit default paths that are mounted, and the
// listeners moved to loopback ports.
func (r *podRunner) containerArgs(p *localPod, pc parsedContainer) ([]string, error) {
	c := pc.c
	argv := append(slices.Clone(c.Command), c.Args...)
	envMap := map[string]string{}
	for _, e := range c.Env {
		if e.ValueFrom == nil {
			envMap[e.Name] = e.Value
		}
	}
	var args []string
	for _, a := range argv[1:] {
		a = expand(a, envMap)
		if name, val, ok := strings.Cut(a, "="); ok && strings.HasPrefix(name, "-") {
			a = name + "=" + r.rewrite(p, c, val)
		} else {
			a = r.rewrite(p, c, a)
		}
		args = append(args, a)
	}
	implicit := func(flag, def string) {
		if pc.fs.Lookup(flag) == nil || pc.fs.Changed(flag) {
			return
		}
		if hp, ok := p.hostPath(c, def); ok {
			args = append(args, "--"+flag+"="+hp)
		}
	}
	switch pc.sub {
	case "repo-server":
		implicit("ssh-known-hosts-path", reposerver.DefaultSSHKnownHostsPath)
		implicit("tls-certs-dir", reposerver.DefaultTLSCertsDir)
		implicit("gpg-keys-dir", reposerver.DefaultGPGKeysDir)
		implicit("plugin-socket-dir", cmpwire.DefaultSocketDir)
		if !pc.fs.Changed("tls-cert") {
			crt, ok1 := p.hostPath(c, "/app/config/reposerver/tls/tls.crt")
			key, ok2 := p.hostPath(c, "/app/config/reposerver/tls/tls.key")
			if ok1 && ok2 && fileExists(crt) && fileExists(key) {
				args = append(args, "--tls-cert="+crt, "--tls-key="+key)
			}
		}
	case "commit-server":
		implicit("ssh-known-hosts-path", reposerver.DefaultSSHKnownHostsPath)
		implicit("tls-certs-dir", reposerver.DefaultTLSCertsDir)
	case "cmp-server":
		implicit("socket-dir", cmpwire.DefaultSocketDir)
	}
	ls, err := pc.spec.listeners(pc.fs)
	if err != nil {
		return nil, err
	}
	for _, l := range ls {
		local, ok := p.ports[int32(l.port)]
		if !ok {
			return nil, fmt.Errorf("%s: listener %s port %d is not a containerPort", pc.id(), l.name, l.port)
		}
		addr := "127.0.0.1:" + strconv.Itoa(local)
		switch l.name {
		case "api":
			args = append(args, "--address=127.0.0.1", "--port="+strconv.Itoa(local))
		case "metrics":
			args = append(args, "--metrics-addr="+addr)
		case "healthz":
			args = append(args, "--healthz-addr="+addr)
		case "tree-api":
			args = append(args, "--tree-api-addr="+addr)
		case "webhook":
			args = append(args, "--webhook-addr="+addr)
		}
	}
	if pc.sub == "server" {
		if ctl := r.pod(flagValue(pc.fs, "controller-statefulset")); ctl != nil {
			port, err := strconv.Atoi(flagValue(pc.fs, "controller-tree-api-port"))
			if err != nil {
				return nil, err
			}
			scheme := "http"
			if flagValue(pc.fs, "controller-tree-api-tls") == "true" {
				scheme = "https"
			}
			args = append(args, fmt.Sprintf("--controller-tree-api-url=%s://127.0.0.1:%d", scheme, ctl.ports[int32(port)]))
		}
	}
	args = append(args, r.extraArgs[c.Name]...)
	return args, nil
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

// start sets up the pod's volumes (retrying while a required Secret or ConfigMap is
// missing, like the kubelet), runs the init containers to completion, then starts the
// containers. It does not wait for readiness.
func (r *podRunner) start(ctx context.Context, p *localPod) error {
	for {
		err := r.setupVolumes(ctx, p)
		if err == nil {
			break
		}
		var nr errVolumeNotReady
		if !errors.As(err, &nr) {
			return fmt.Errorf("%s: %w", p.name, err)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s: volumes never became ready: %w", p.name, err)
		case <-time.After(300 * time.Millisecond):
		}
	}
	pt := &podTemplate{owner: p.owner, tmpl: p.tmpl}
	for _, c := range p.tmpl.Spec.InitContainers {
		if !isPatrolcd(c) {
			return fmt.Errorf("%s: init container %s does not run patrolcd (not supported by the harness)", p.name, c.Name)
		}
		proc, err := r.launch(ctx, p, pt, c, true)
		if err != nil {
			return err
		}
		p.inits = append(p.inits, proc)
		select {
		case <-proc.done:
		case <-ctx.Done():
			return fmt.Errorf("%s: init container did not finish", proc.name())
		}
		if proc.err != nil {
			return fmt.Errorf("init container %s failed: %v\n%s", proc.name(), proc.err, proc.log.String())
		}
	}
	for _, c := range p.tmpl.Spec.Containers {
		if !isPatrolcd(c) {
			r.skipped = append(r.skipped, p.name+"/"+c.Name)
			continue
		}
		proc, err := r.launch(ctx, p, pt, c, false)
		if err != nil {
			return err
		}
		p.procs = append(p.procs, proc)
	}
	return nil
}

// launch starts one container process.
func (r *podRunner) launch(ctx context.Context, p *localPod, pt *podTemplate, c corev1.Container, init bool) (*localProc, error) {
	pc, err := parseContainer(pt, c, init)
	if err != nil {
		return nil, err
	}
	var env []string
	for {
		env, err = r.containerEnv(ctx, p, pc)
		var nr errVolumeNotReady
		if err == nil || !errors.As(err, &nr) {
			break
		}
		select {
		case <-ctx.Done():
			return nil, err
		case <-time.After(300 * time.Millisecond):
		}
	}
	if err != nil {
		return nil, fmt.Errorf("%s/%s: environment: %w", p.name, c.Name, err)
	}
	args, err := r.containerArgs(p, pc)
	if err != nil {
		return nil, err
	}
	proc := &localProc{pod: p, c: c, pc: pc, args: args, env: env, log: &syncBuffer{}, done: make(chan struct{})}
	cmd := exec.Command(r.binary, args...)
	cmd.Env = env
	cmd.Dir = p.dir
	var out io.Writer = proc.log
	if dir := os.Getenv(envKeepLogs); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err == nil {
			f, err := os.Create(filepath.Join(dir, strings.NewReplacer("/", "_").Replace(r.t.Name()+"_"+proc.name())+".log"))
			if err == nil {
				r.t.Cleanup(func() { _ = f.Close() })
				out = io.MultiWriter(proc.log, f)
			}
		}
	}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", proc.name(), err)
	}
	proc.cmd = cmd
	go func() {
		proc.err = cmd.Wait()
		close(proc.done)
	}()
	return proc, nil
}

// probeURL returns the URL of a container's HTTP probe on loopback.
func (p *localPod) probeURL(c corev1.Container, pr *corev1.Probe) (string, error) {
	if pr == nil || pr.HTTPGet == nil {
		return "", errors.New("no httpGet probe")
	}
	port := int32(pr.HTTPGet.Port.IntValue())
	if pr.HTTPGet.Port.Type == intstr.String {
		port = -1
		for _, cp := range c.Ports {
			if cp.Name == pr.HTTPGet.Port.StrVal {
				port = cp.ContainerPort
			}
		}
	}
	local, ok := p.ports[port]
	if !ok {
		return "", fmt.Errorf("probe port %s is not mapped", pr.HTTPGet.Port.String())
	}
	scheme := strings.ToLower(string(pr.HTTPGet.Scheme))
	if scheme == "" {
		scheme = "http"
	}
	return fmt.Sprintf("%s://127.0.0.1:%d%s", scheme, local, pr.HTTPGet.Path), nil
}

var probeHTTP = &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}} //nolint:gosec // kubelet probes do not verify certificates either

func probe(url string) (int, string) {
	resp, err := probeHTTP.Get(url)
	if err != nil {
		return 0, err.Error()
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return resp.StatusCode, strings.TrimSpace(string(b))
}

// waitReady waits until every container's readiness probe answers 200 (as the kubelet would
// mark it Ready), then checks that the liveness and startup probes answer 200 too.
func (r *podRunner) waitReady(t testing.TB, p *localPod, timeout time.Duration) {
	t.Helper()
	for _, proc := range p.procs {
		if proc.c.ReadinessProbe == nil {
			continue
		}
		url, err := p.probeURL(proc.c, proc.c.ReadinessProbe)
		if err != nil {
			t.Fatalf("%s: readiness probe: %v", proc.name(), err)
		}
		waitFor(t, timeout, proc.name()+" readiness "+url, func() (bool, string) {
			select {
			case <-proc.done:
				t.Fatalf("%s exited before it was ready: %v\n%s", proc.name(), proc.err, tail(proc.log.String(), 8000))
			default:
			}
			code, body := probe(url)
			return code == http.StatusOK, fmt.Sprintf("%d %s", code, body)
		})
		for typ, pr := range map[string]*corev1.Probe{"liveness": proc.c.LivenessProbe, "startup": proc.c.StartupProbe} {
			if pr == nil {
				continue
			}
			u, err := p.probeURL(proc.c, pr)
			if err != nil {
				t.Fatalf("%s: %s probe: %v", proc.name(), typ, err)
			}
			if code, body := probe(u); code != http.StatusOK {
				t.Errorf("%s: %s probe %s answered %d %s", proc.name(), typ, u, code, body)
			}
		}
	}
}

// stop sends SIGTERM to the pod's containers and waits up to the pod's
// terminationGracePeriodSeconds; the process must exit with status 0 (clean drain).
func (r *podRunner) stop(t testing.TB, p *localPod) {
	t.Helper()
	grace := 30 * time.Second
	if g := p.tmpl.Spec.TerminationGracePeriodSeconds; g != nil {
		grace = time.Duration(*g) * time.Second
	}
	for _, proc := range p.procs {
		select {
		case <-proc.done:
			t.Errorf("%s exited before SIGTERM: %v\n%s", proc.name(), proc.err, tail(proc.log.String(), 4000))
			continue
		default:
		}
		start := time.Now()
		proc.stopped = true
		_ = proc.cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-proc.done:
			proc.stopDur = time.Since(start)
			if proc.err != nil {
				t.Errorf("%s: exit after SIGTERM: %v (want status 0)\n%s", proc.name(), proc.err, tail(proc.log.String(), 4000))
			}
		case <-time.After(grace):
			_ = proc.cmd.Process.Kill()
			<-proc.done
			t.Errorf("%s did not exit within terminationGracePeriodSeconds (%s) of SIGTERM", proc.name(), grace)
		}
	}
}

// killAll kills every process still running (test cleanup).
func (r *podRunner) killAll() {
	for _, p := range r.pods {
		for _, proc := range append(slices.Clone(p.inits), p.procs...) {
			if proc.cmd == nil {
				continue
			}
			select {
			case <-proc.done:
			default:
				_ = proc.cmd.Process.Kill()
				<-proc.done
			}
		}
	}
}

// allProcs returns every started process (init containers included).
func (r *podRunner) allProcs() []*localProc {
	var out []*localProc
	for _, p := range r.pods {
		out = append(out, p.inits...)
		out = append(out, p.procs...)
	}
	return out
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

// commandLine renders a process for the run record (no secret values are in arguments).
func (p *localProc) commandLine() string {
	return path.Base(p.cmd.Path) + " " + strings.Join(p.args, " ")
}
