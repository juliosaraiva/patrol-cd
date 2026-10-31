package deploy_test

import (
	"fmt"
	"io/fs"
	"net"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/juliosaraiva/patrol-cd/internal/clusters"
	"github.com/juliosaraiva/patrol-cd/internal/cmp"
	"github.com/juliosaraiva/patrol-cd/internal/controller/sharding"
	"github.com/juliosaraiva/patrol-cd/internal/render/cmpwire"
	"github.com/juliosaraiva/patrol-cd/internal/reposerver"
	"github.com/juliosaraiva/patrol-cd/internal/server"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
)

// The expectations below encode docs/architecture/components/wiring.md ("Ports", "Environment
// variables", "Deployment requirements") and bootstrap.md ("Deployment requirements"). Code
// constants are used where the packages export them, so a renamed path or variable shows up
// here as a compile error or a failing check.

// listenerKind tells which probe paths a listener serves.
type listenerKind string

const (
	lkAPI      listenerKind = "api"     // patrol-server API: /healthz only (also under --rootpath)
	lkMetrics  listenerKind = "metrics" // metrics + /healthz, /readyz, /livez
	lkRepoMet  listenerKind = "repo-metrics"
	lkHealthz  listenerKind = "healthz"  // separate probe listener
	lkInternal listenerKind = "internal" // repo-server/commit-server API, tree API: no kubelet probes
	lkWebhook  listenerKind = "webhook"  // appset SCM webhooks
)

// listener is one TCP listener a component opens.
type listener struct {
	name string
	kind listenerKind
	port int
}

// allowedProbePaths per listener kind and probe type: readiness evaluates readiness;
// liveness must not (a restart does not fix a missing dependency), so it uses /livez where
// the listener has it (the repo-server's probe listener has no /livez: /healthz?full=true
// dials its own API listener, as upstream).
var allowedProbePaths = map[listenerKind]map[string][]string{
	lkMetrics: {"readiness": {"/readyz", "/healthz"}, "liveness": {"/livez"}, "startup": {"/livez", "/readyz", "/healthz"}},
	lkHealthz: {"readiness": {"/readyz", "/healthz"}, "liveness": {"/livez"}, "startup": {"/livez", "/readyz", "/healthz"}},
	lkRepoMet: {"readiness": {"/readyz", "/healthz"}, "liveness": {"/healthz", "/healthz?full=true"}, "startup": {"/healthz", "/healthz?full=true", "/readyz"}},
	lkAPI:     {"readiness": {"/healthz"}, "liveness": {"/healthz"}, "startup": {"/healthz"}},
}

// subcommand describes one patrolcd subcommand as deployed.
type subcommand struct {
	settings settings.Component // "" when the command reads no patrol-cmd-params-cm keys
	// kube: the command talks to the Kubernetes API (service account token, POD_NAMESPACE).
	kube bool
	// token: the command reads the internal token file.
	token bool
	// listeners computes the listeners from the parsed flags.
	listeners func(fs *pflag.FlagSet) ([]listener, error)
	// env are the documented PATROL_* variables the command reads besides its parameters.
	env []string
	// profileKey is the patrol-cmd-params-cm key projected to the profiler file ("" none).
	profileKey string
	// noScratch: the command writes nothing to disk (no /tmp volume needed).
	noScratch bool
}

var (
	envAll         = []string{server.EnvBinaryName}
	envToken       = []string{server.EnvInternalTokenFile}
	envProfiler    = []string{server.EnvProfilerFile}
	envClusterTune = []string{"PATROL_K8S_CLIENT_QPS", "PATROL_K8S_CLIENT_BURST", "PATROL_K8S_CLIENT_MAX_IDLE_CONNECTIONS", "PATROL_K8S_TCP_TIMEOUT", "PATROL_K8S_TCP_KEEPALIVE", "PATROL_K8S_TLS_HANDSHAKE_TIMEOUT", "PATROL_K8S_TCP_IDLE_TIMEOUT", "PATROL_K8S_SERVER_SIDE_TIMEOUT", "PATROL_K8SCLIENT_RETRY_MAX", "PATROL_K8SCLIENT_RETRY_BASE_BACKOFF"}
	envGPG         = []string{"PATROL_GPG_ENABLED"}
	envSharding    = []string{sharding.EnvControllerReplicas, sharding.EnvControllerShard, sharding.EnvDynamicClusterDistribution, sharding.EnvHeartbeatTime, sharding.EnvApplicationControllerName}
	envLogin       = []string{"PATROL_SESSION_FAILURE_MAX_FAIL_COUNT", "PATROL_SESSION_FAILURE_WINDOW_SECONDS", "PATROL_SESSION_MAX_CACHE_SIZE", "PATROL_MAX_CONCURRENT_LOGIN_REQUESTS_COUNT", "PATROL_APPLICATIONSET_CONTROLLER_NAMESPACES"}
	envRepoTuning  = []string{"PATROL_EXEC_TIMEOUT", "PATROL_EXEC_FATAL_TIMEOUT", "PATROL_GIT_ATTEMPTS_COUNT", "PATROL_GIT_RETRY_DURATION", "PATROL_GIT_RETRY_MAX_DURATION", "PATROL_PAUSE_GEN_AFTER_FAILED_ATTEMPTS", "PATROL_PAUSE_GEN_MINUTES", "PATROL_PAUSE_GEN_REQUESTS"}
	envCMP         = []string{cmp.EnvSocketDir, cmp.EnvSocketDirArgo, cmp.EnvWorkDir, cmp.EnvWorkDirArgo, cmp.EnvExecTimeout, cmp.EnvExecTimeoutArgo, cmp.EnvPassEnv, cmp.EnvReceiveTimeout, cmp.EnvMaxArchiveSize, cmp.EnvMaxExtractedSize, cmp.EnvMaxFiles, cmp.EnvLogLevel, cmp.EnvLogLevelArgo, cmp.EnvLogFormat, cmp.EnvLogFormatArgo}
	envNeverDeploy = []string{clusters.EnvFakeInCluster, "PATROL_GIT_ASKPASS_FILE", "PATROL_GIT_ASKPASS_ORIGIN"}
)

func concat(lists ...[]string) []string {
	var out []string
	for _, l := range lists {
		out = append(out, l...)
	}
	return out
}

var subcommands = map[string]subcommand{
	"server": {
		settings: settings.ComponentServer, kube: true, token: true, profileKey: "server.profile.enabled",
		env: concat(envAll, envToken, envProfiler, envClusterTune, envGPG, envLogin),
		listeners: func(fs *pflag.FlagSet) ([]listener, error) {
			api, err := intFlag(fs, "port")
			if err != nil {
				return nil, err
			}
			out := []listener{{name: "api", kind: lkAPI, port: api}}
			return withMetricsHealthz(fs, out, lkMetrics, "metrics-port")
		},
	},
	"controller": {
		settings: settings.ComponentController, kube: true, token: true, profileKey: "controller.profile.enabled",
		env: concat(envAll, envToken, envProfiler, envClusterTune, envGPG, envSharding, []string{clusters.EnvUpdateClusterInfoTimeout}),
		listeners: func(fs *pflag.FlagSet) ([]listener, error) {
			out, err := withMetricsHealthz(fs, nil, lkMetrics, "metrics-port")
			if err != nil {
				return nil, err
			}
			if p, on, err := addrPort(flagValue(fs, "tree-api-addr")); err != nil {
				return nil, fmt.Errorf("--tree-api-addr: %w", err)
			} else if on {
				out = append(out, listener{name: "tree-api", kind: lkInternal, port: p})
			}
			return out, nil
		},
	},
	"repo-server": {
		settings: settings.ComponentRepoServer, token: true, profileKey: "reposerver.profile.enabled",
		env: concat(envAll, envToken, envProfiler, envGPG, envRepoTuning),
		listeners: func(fs *pflag.FlagSet) ([]listener, error) {
			api, err := intFlag(fs, "port")
			if err != nil {
				return nil, err
			}
			return withMetricsHealthz(fs, []listener{{name: "api", kind: lkInternal, port: api}}, lkRepoMet, "metrics-port")
		},
	},
	"appset-controller": {
		settings: settings.ComponentAppSetController, kube: true, token: true, profileKey: "applicationsetcontroller.profile.enabled",
		env: concat(envAll, envToken, envProfiler),
		listeners: func(fs *pflag.FlagSet) ([]listener, error) {
			out, err := withMetricsHealthz(fs, nil, lkMetrics, "")
			if err != nil {
				return nil, err
			}
			if p, on, err := addrPort(flagValue(fs, "webhook-addr")); err != nil {
				return nil, fmt.Errorf("--webhook-addr: %w", err)
			} else if on {
				out = append(out, listener{name: "webhook", kind: lkWebhook, port: p})
			}
			return out, nil
		},
	},
	"notifications-controller": {
		settings: settings.ComponentNotificationsController, kube: true, token: true,
		env: concat(envAll, envToken, envProfiler),
		listeners: func(fs *pflag.FlagSet) ([]listener, error) {
			return withMetricsHealthz(fs, nil, lkMetrics, "")
		},
	},
	"commit-server": {
		settings: settings.ComponentCommitServer, token: true,
		env: concat(envAll, envToken, envProfiler),
		listeners: func(fs *pflag.FlagSet) ([]listener, error) {
			api, err := intFlag(fs, "port")
			if err != nil {
				return nil, err
			}
			return withMetricsHealthz(fs, []listener{{name: "api", kind: lkInternal, port: api}}, lkMetrics, "metrics-port")
		},
	},
	"cmp-server": {
		env: concat(envAll, envCMP),
		listeners: func(fs *pflag.FlagSet) ([]listener, error) {
			return withMetricsHealthz(fs, nil, lkMetrics, "")
		},
	},
	"bootstrap": {
		kube: true, env: envAll, noScratch: true,
		listeners: func(*pflag.FlagSet) ([]listener, error) { return nil, nil },
	},
}

func flagValue(fs *pflag.FlagSet, name string) string {
	if f := fs.Lookup(name); f != nil {
		return f.Value.String()
	}
	return ""
}

func intFlag(fs *pflag.FlagSet, name string) (int, error) {
	n, err := strconv.Atoi(flagValue(fs, name))
	if err != nil {
		return 0, fmt.Errorf("--%s: %w", name, err)
	}
	return n, nil
}

// addrPort returns the port of a listen address; "" and "-" mean disabled.
func addrPort(addr string) (int, bool, error) {
	if addr == "" || addr == "-" {
		return 0, false, nil
	}
	_, p, err := net.SplitHostPort(addr)
	if err != nil {
		return 0, false, err
	}
	n, err := strconv.Atoi(p)
	if err != nil || n <= 0 || n > 65535 {
		return 0, false, fmt.Errorf("invalid port in %q", addr)
	}
	return n, true, nil
}

// withMetricsHealthz adds the metrics listener (--metrics-addr, else the port flag) and the
// optional --healthz-addr listener.
func withMetricsHealthz(fs *pflag.FlagSet, out []listener, metricsKind listenerKind, portFlag string) ([]listener, error) {
	addr := flagValue(fs, "metrics-addr")
	if addr == "" && portFlag != "" {
		p, err := intFlag(fs, portFlag)
		if err != nil {
			return nil, err
		}
		out = append(out, listener{name: "metrics", kind: metricsKind, port: p})
	} else if p, on, err := addrPort(addr); err != nil {
		return nil, fmt.Errorf("--metrics-addr: %w", err)
	} else if on {
		out = append(out, listener{name: "metrics", kind: metricsKind, port: p})
	}
	if fs.Lookup("healthz-addr") != nil {
		if p, on, err := addrPort(flagValue(fs, "healthz-addr")); err != nil {
			return nil, fmt.Errorf("--healthz-addr: %w", err)
		} else if on {
			out = append(out, listener{name: "healthz", kind: lkHealthz, port: p})
		}
	}
	return out, nil
}

// parsedContainer is a patrolcd container with its arguments parsed by the real command.
type parsedContainer struct {
	pod    *podTemplate
	c      corev1.Container
	init   bool
	sub    string
	spec   subcommand
	cmd    *cobra.Command
	fs     *pflag.FlagSet
	params map[string]settings.ParamSpec // by environment variable name
}

func (pc parsedContainer) id() string { return pc.pod.owner + "/" + pc.c.Name }

// isPatrolcd reports whether a container runs the patrolcd binary (its argv[0] base name).
func isPatrolcd(c corev1.Container) bool {
	argv := append(slices.Clone(c.Command), c.Args...)
	return len(argv) > 0 && path.Base(argv[0]) == server.BinaryName
}

// parseContainer parses a patrolcd container's arguments with a fresh command tree.
func parseContainer(pt *podTemplate, c corev1.Container, init bool) (parsedContainer, error) {
	argv := append(slices.Clone(c.Command), c.Args...)
	pc := parsedContainer{pod: pt, c: c, init: init}
	if len(argv) < 2 {
		return pc, fmt.Errorf("no subcommand in %q", argv)
	}
	root := server.NewRootCommand()
	cmd, rest, err := root.Find(argv[1:])
	if err != nil {
		return pc, fmt.Errorf("find subcommand of %q: %w", argv, err)
	}
	if cmd == root {
		return pc, fmt.Errorf("%q is not a patrolcd subcommand", argv[1])
	}
	pc.sub, pc.cmd = cmd.Name(), cmd
	spec, ok := subcommands[pc.sub]
	if !ok {
		return pc, fmt.Errorf("subcommand %q has no deployment expectations in this test", pc.sub)
	}
	pc.spec = spec
	if err := cmd.ParseFlags(rest); err != nil {
		return pc, fmt.Errorf("patrolcd %s: %w", pc.sub, err)
	}
	if err := cmd.ValidateArgs(cmd.Flags().Args()); err != nil {
		return pc, fmt.Errorf("patrolcd %s: positional arguments %q: %w", pc.sub, cmd.Flags().Args(), err)
	}
	pc.fs = cmd.Flags()
	pc.params = map[string]settings.ParamSpec{}
	if spec.settings != "" {
		for _, s := range settings.ParamsForComponent(spec.settings) {
			if s.Env != "" {
				pc.params[s.Env] = s
			}
		}
	}
	return pc, nil
}

// wiringReport accumulates the results of one profile.
type wiringReport struct {
	problems   []string
	notes      []string
	containers int
	others     []string // non-patrolcd containers (not parsed)
}

func (r *wiringReport) fail(format string, a ...any) {
	r.problems = append(r.problems, fmt.Sprintf(format, a...))
}
func (r *wiringReport) note(format string, a ...any) {
	r.notes = append(r.notes, fmt.Sprintf(format, a...))
}

// checkWiring runs every wiring check on a rendered profile.
func checkWiring(t testing.TB, objs []object) *wiringReport {
	t.Helper()
	r := &wiringReport{}
	pts := podTemplates(t, objs)
	cms := renderedData(objs, "ConfigMap")
	secrets := renderedData(objs, "Secret")
	var all []parsedContainer
	byPod := make([][]parsedContainer, len(pts))
	for i := range pts {
		pt := &pts[i]
		for j, c := range append(slices.Clone(pt.tmpl.Spec.InitContainers), pt.tmpl.Spec.Containers...) {
			init := j < len(pt.tmpl.Spec.InitContainers)
			if !isPatrolcd(c) {
				r.others = append(r.others, pt.owner+"/"+c.Name)
				continue
			}
			pc, err := parseContainer(pt, c, init)
			if err != nil {
				r.fail("%s/%s: %v", pt.owner, c.Name, err)
				continue
			}
			r.containers++
			byPod[i] = append(byPod[i], pc)
		}
		all = append(all, byPod[i]...)
	}
	// The bootstrap seeds ssh_known_hosts into the shipped known hosts ConfigMap at runtime
	// unless it runs with --skip-ssh-known-hosts (bootstrap.md): the key counts as provided.
	for _, pc := range all {
		if pc.sub != "bootstrap" || flagValue(pc.fs, "skip-ssh-known-hosts") == "true" {
			continue
		}
		ns := strings.Split(pc.pod.owner, "/")[1]
		if keys, ok := cms[ns+"/"+settings.DefaultSSHKnownHostsConfigMapName]; ok {
			keys[settings.SSHKnownHostsKey] = true
		} else {
			r.fail("%s: seeds the default known hosts but %s is not rendered (pass --skip-ssh-known-hosts)", pc.id(), settings.DefaultSSHKnownHostsConfigMapName)
		}
	}
	for i := range pts {
		checkPod(r, &pts[i], byPod[i], cms, secrets)
	}
	checkCrossPod(r, all)
	checkServices(r, objs, pts)
	return r
}

// renderedData returns the data keys of rendered ConfigMaps or Secrets by namespace/name.
func renderedData(objs []object, kind string) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	for _, o := range byKind(objs, kind) {
		keys := map[string]bool{}
		for _, field := range []string{"data", "stringData", "binaryData"} {
			if m, ok := o.u.Object[field].(map[string]any); ok {
				for key := range m {
					keys[key] = true
				}
			}
		}
		out[o.u.GetNamespace()+"/"+o.u.GetName()] = keys
	}
	return out
}

// checkPod verifies one pod template's patrolcd containers.
func checkPod(r *wiringReport, pt *podTemplate, pcs []parsedContainer, cms, secrets map[string]map[string]bool) {
	spec := pt.tmpl.Spec
	ns := strings.Split(pt.owner, "/")[1]
	vols := map[string]corev1.Volume{}
	for _, v := range spec.Volumes {
		vols[v.Name] = v
	}
	needsKube := false
	bootstrapRuns := false
	for _, pc := range pcs {
		if pc.spec.kube {
			needsKube = true
		}
		if pc.sub == "bootstrap" {
			bootstrapRuns = true
			if !pc.init {
				r.fail("%s: bootstrap must run as an init container", pc.id())
			}
		}
	}
	automount := spec.AutomountServiceAccountToken != nil && *spec.AutomountServiceAccountToken
	if needsKube && !automount {
		r.fail("%s: runs a Kubernetes-facing component but automountServiceAccountToken is not true", pt.owner)
	}
	if !needsKube && len(pcs) > 0 && automount {
		r.fail("%s: no component needs the Kubernetes API but the service account token is mounted", pt.owner)
	}
	fc := fileChecker{r: r, vols: vols, cms: cms, secrets: secrets, ns: ns}
	for _, pc := range pcs {
		checkArgsAndEnv(r, pc)
		checkListeners(r, pc)
		// Writable scratch space: the root filesystem is read-only, TMPDIR defaults to /tmp.
		if m, rel := mountFor(pc.c, "/tmp/x"); !pc.spec.noScratch && (m == nil || rel != "x" || vols[m.Name].EmptyDir == nil || m.ReadOnly) {
			r.fail("%s: /tmp is not a writable emptyDir mount", pc.id())
		}
		if pc.spec.kube && !hasFieldRefEnv(pc.c, "POD_NAMESPACE", "metadata.namespace") {
			r.fail("%s: POD_NAMESPACE from fieldRef metadata.namespace is missing", pc.id())
		}
		// Every absolute path given on the command line must be provided by a volume (the root
		// filesystem is the image's, read-only); writable directories must be emptyDirs.
		pc.fs.Visit(func(f *pflag.Flag) {
			v := f.Value.String()
			if f.Value.Type() != "string" || !strings.HasPrefix(v, "/") {
				return
			}
			writable := slices.Contains([]string{"root-dir", "credentials-dir", "gnupg-home-parent", "work-dir"}, f.Name)
			fc.covered(pc, "--"+f.Name, v, writable, isDirFlag(f.Name))
		})
		if pc.spec.token {
			p := effectivePath(pc, "internal-token-file", server.EnvInternalTokenFile, "", server.DefaultInternalTokenFile)
			src := fc.covered(pc, "internal token", p, false, false)
			if bootstrapRuns && pc.fs.Changed("internal-token-file") {
				r.fail("%s: runs next to the bootstrap init container but passes --internal-token-file, which disables the Secret fallback on the first start (bootstrap.md)", pc.id())
			}
			if bootstrapRuns && src.volume != nil && !bootstrapSkipsToken(pcs) {
				if src.volume.Secret == nil || src.volume.Secret.SecretName != server.InternalTokenSecretName {
					r.fail("%s: the bootstrap creates Secret %s but the token volume %s does not reference it", pc.id(), server.InternalTokenSecretName, src.volume.Name)
				} else if src.volume.Secret.Optional == nil || !*src.volume.Secret.Optional {
					r.fail("%s: token volume %s must be optional: the bootstrap init container creates the Secret after volumes are set up (bootstrap.md)", pc.id(), src.volume.Name)
				}
			}
		}
		if pc.spec.profileKey != "" {
			qfc := fc
			qfc.quiet = true
			src := qfc.covered(pc, "profiler toggle", server.DefaultProfilerFile, false, false)
			if src.volume != nil {
				if src.volume.ConfigMap == nil || src.volume.ConfigMap.Name != settings.DefaultCmdParamsConfigMapName || src.key != pc.spec.profileKey {
					r.fail("%s: %s must project %s key %s, got volume %s key %q", pc.id(), server.DefaultProfilerFile, settings.DefaultCmdParamsConfigMapName, pc.spec.profileKey, src.volume.Name, src.key)
				}
			}
		}
		switch pc.sub {
		case "repo-server", "commit-server":
			checkTrust(fc, pc, "ssh-known-hosts-path", reposerver.DefaultSSHKnownHostsPath, "patrol-ssh-known-hosts-cm", "ssh_known_hosts", false)
			checkTrust(fc, pc, "tls-certs-dir", reposerver.DefaultTLSCertsDir, "patrol-tls-certs-cm", "", true)
			if pc.sub == "repo-server" {
				checkTrust(fc, pc, "gpg-keys-dir", reposerver.DefaultGPGKeysDir, "patrol-gpg-keys-cm", "", true)
			}
		}
	}
	checkCMPSidecars(r, pcs, fc)
}

// bootstrapSkipsToken reports whether the pod's bootstrap runs with --skip-internal-token.
func bootstrapSkipsToken(pcs []parsedContainer) bool {
	for _, pc := range pcs {
		if pc.sub == "bootstrap" && flagValue(pc.fs, "skip-internal-token") == "true" {
			return true
		}
	}
	return false
}

func isDirFlag(name string) bool {
	return strings.HasSuffix(name, "-dir") || strings.HasSuffix(name, "-parent") || name == "config-dir-path"
}

func hasFieldRefEnv(c corev1.Container, name, fieldPath string) bool {
	for _, e := range c.Env {
		if e.Name == name && e.ValueFrom != nil && e.ValueFrom.FieldRef != nil && e.ValueFrom.FieldRef.FieldPath == fieldPath {
			return true
		}
	}
	return false
}

// effectivePath is the path a component uses: the flag when given on the command line, else
// a literal environment variable of the container, else the default.
func effectivePath(pc parsedContainer, flag, env, cmKey, def string) string {
	if flag != "" && pc.fs.Changed(flag) {
		return flagValue(pc.fs, flag)
	}
	if env != "" {
		for _, e := range pc.c.Env {
			if e.Name == env && e.ValueFrom == nil && e.Value != "" {
				return e.Value
			}
		}
	}
	return def
}

// checkTrust verifies a trust-material mount (known hosts, TLS certificates, GnuPG keys).
func checkTrust(fc fileChecker, pc parsedContainer, flag, def, cm, key string, dir bool) {
	p := effectivePath(pc, flag, "", "", def)
	if p == reposerver.TrustMaterialDisabled {
		fc.r.note("%s: --%s disabled", pc.id(), flag)
		return
	}
	src := fc.covered(pc, "--"+flag, p, false, dir)
	if src.volume == nil {
		return
	}
	if src.volume.ConfigMap == nil || src.volume.ConfigMap.Name != cm {
		fc.r.fail("%s: %s is mounted from volume %s, want ConfigMap %s", pc.id(), p, src.volume.Name, cm)
		return
	}
	if key != "" {
		if keys, ok := fc.cms[fc.ns+"/"+cm]; ok && !keys[key] {
			fc.r.fail("%s: ConfigMap %s has no key %q (read from %s)", pc.id(), cm, key, p)
		}
	}
}

// fileSource is the volume (and key) providing a path.
type fileSource struct {
	volume *corev1.Volume
	key    string
}

type fileChecker struct {
	// quiet suppresses the note about keys absent from optional volumes (expected for
	// toggles that are off by default, such as the profiler).
	quiet   bool
	r       *wiringReport
	vols    map[string]corev1.Volume
	cms     map[string]map[string]bool
	secrets map[string]map[string]bool
	ns      string
}

// mountFor returns the mount whose path is the longest prefix of p, and p relative to it.
func mountFor(c corev1.Container, p string) (*corev1.VolumeMount, string) {
	var best *corev1.VolumeMount
	rel := ""
	for i := range c.VolumeMounts {
		m := &c.VolumeMounts[i]
		mp := strings.TrimSuffix(m.MountPath, "/")
		if p != mp && !strings.HasPrefix(p, mp+"/") {
			continue
		}
		if best == nil || len(mp) > len(strings.TrimSuffix(best.MountPath, "/")) {
			best = m
			rel = strings.TrimPrefix(strings.TrimPrefix(p, mp), "/")
		}
	}
	return best, rel
}

// covered checks that path p is provided by a volume of the container: for a file in a
// Secret or ConfigMap volume, an item (or, without items, a key of the rendered object)
// provides it; writable paths must be emptyDirs. It returns the source (nil volume when not
// covered, after recording the problem).
func (fc fileChecker) covered(pc parsedContainer, what, p string, writable, dir bool) fileSource {
	m, rel := mountFor(pc.c, p)
	if m == nil {
		fc.r.fail("%s: %s %s is not provided by any volume mount", pc.id(), what, p)
		return fileSource{}
	}
	v, ok := fc.vols[m.Name]
	if !ok {
		fc.r.fail("%s: mount %s references unknown volume %s", pc.id(), m.MountPath, m.Name)
		return fileSource{}
	}
	src := fileSource{volume: &v}
	if m.SubPath != "" {
		rel = path.Join(m.SubPath, rel)
	}
	if writable {
		if v.EmptyDir == nil || m.ReadOnly {
			fc.r.fail("%s: %s %s must be a writable emptyDir (volume %s)", pc.id(), what, p, v.Name)
		}
		return src
	}
	if dir || rel == "" {
		return src
	}
	var items []corev1.KeyToPath
	var objKeys map[string]bool
	optional := false
	switch {
	case v.Secret != nil:
		items = v.Secret.Items
		objKeys = fc.secrets[fc.ns+"/"+v.Secret.SecretName]
		optional = v.Secret.Optional != nil && *v.Secret.Optional
	case v.ConfigMap != nil:
		items = v.ConfigMap.Items
		objKeys = fc.cms[fc.ns+"/"+v.ConfigMap.Name]
		optional = v.ConfigMap.Optional != nil && *v.ConfigMap.Optional
	case v.Projected != nil:
		for _, s := range v.Projected.Sources {
			switch {
			case s.Secret != nil:
				items = append(items, s.Secret.Items...)
			case s.ConfigMap != nil:
				items = append(items, s.ConfigMap.Items...)
			}
		}
	case v.EmptyDir != nil:
		return src
	default:
		fc.r.fail("%s: %s %s comes from an unsupported volume type (%s)", pc.id(), what, p, v.Name)
		return fileSource{}
	}
	// A key missing from a rendered object keeps the pod from starting unless the volume is
	// optional (then the file is simply absent, which the components treat as "not set").
	missing := func(key string) {
		if objKeys == nil || objKeys[key] {
			return
		}
		if optional {
			if fc.quiet {
				return
			}
			fc.r.note("%s: %s %s: key %q absent from the rendered object of optional volume %s (file absent)", pc.id(), what, p, key, v.Name)
			return
		}
		fc.r.fail("%s: %s %s: rendered object of volume %s has no key %q and the volume is not optional (the pod would not start)", pc.id(), what, p, v.Name, key)
	}
	if len(items) == 0 {
		missing(rel)
		src.key = rel
		return src
	}
	for _, it := range items {
		if it.Path == rel {
			src.key = it.Key
			missing(it.Key)
			return src
		}
	}
	fc.r.fail("%s: %s %s: volume %s has no item with path %q", pc.id(), what, p, v.Name, rel)
	return fileSource{}
}

// checkArgsAndEnv verifies the environment of a parsed container.
func checkArgsAndEnv(r *wiringReport, pc parsedContainer) {
	seen := map[string]bool{}
	for _, e := range pc.c.Env {
		if seen[e.Name] {
			r.fail("%s: duplicate env %s", pc.id(), e.Name)
		}
		seen[e.Name] = true
		if slices.Contains(envNeverDeploy, e.Name) {
			r.fail("%s: %s must never be set in an installation", pc.id(), e.Name)
			continue
		}
		if !strings.HasPrefix(e.Name, "PATROL_") && !strings.HasPrefix(e.Name, "ARGOCD_") {
			continue
		}
		spec, isParam := pc.params[e.Name]
		if !isParam && !slices.Contains(pc.spec.env, e.Name) {
			r.fail("%s: unknown variable %s for patrolcd %s (not a %s parameter, not in wiring.md)", pc.id(), e.Name, pc.sub, pc.spec.settings)
			continue
		}
		if !isParam {
			continue
		}
		if spec.NotApplicable != "" {
			r.note("%s: %s (%s) is projected but has no effect: %s", pc.id(), e.Name, spec.Key, spec.NotApplicable)
		}
		if e.ValueFrom != nil && e.ValueFrom.ConfigMapKeyRef != nil {
			ref := e.ValueFrom.ConfigMapKeyRef
			if ref.Name == settings.DefaultCmdParamsConfigMapName && ref.Key != spec.Key {
				r.fail("%s: %s is projected from %s key %q, but it carries parameter %q", pc.id(), e.Name, ref.Name, ref.Key, spec.Key)
			}
			if ref.Optional == nil || !*ref.Optional {
				r.fail("%s: %s from %s/%s is not optional (a missing key would block the container)", pc.id(), e.Name, ref.Name, ref.Key)
			}
		}
	}
}

// checkListeners verifies container ports and probes against the component's listeners.
func checkListeners(r *wiringReport, pc parsedContainer) {
	ls, err := pc.spec.listeners(pc.fs)
	if err != nil {
		r.fail("%s: %v", pc.id(), err)
		return
	}
	byPort := map[int]listener{}
	for _, l := range ls {
		byPort[l.port] = l
	}
	declared := map[int]bool{}
	names := map[string]int{}
	for _, p := range pc.c.Ports {
		declared[int(p.ContainerPort)] = true
		if p.Name != "" {
			names[p.Name] = int(p.ContainerPort)
		}
		if p.Protocol != "" && p.Protocol != corev1.ProtocolTCP {
			r.fail("%s: port %d uses %s", pc.id(), p.ContainerPort, p.Protocol)
		}
		if _, ok := byPort[int(p.ContainerPort)]; !ok {
			r.fail("%s: containerPort %d (%s) has no listener (listeners: %v)", pc.id(), p.ContainerPort, p.Name, ls)
		}
	}
	for _, l := range ls {
		if !declared[l.port] && !pc.init {
			r.fail("%s: listener %s on port %d is not declared as a containerPort", pc.id(), l.name, l.port)
		}
	}
	probes := map[string]*corev1.Probe{"readiness": pc.c.ReadinessProbe, "liveness": pc.c.LivenessProbe, "startup": pc.c.StartupProbe}
	for _, typ := range []string{"readiness", "liveness", "startup"} {
		pr := probes[typ]
		if pr == nil {
			if typ != "startup" && !pc.init && len(ls) > 0 && pc.sub != "cmp-server" {
				r.fail("%s: no %s probe", pc.id(), typ)
			}
			continue
		}
		if pr.HTTPGet == nil {
			r.note("%s: %s probe is not httpGet", pc.id(), typ)
			continue
		}
		port := pr.HTTPGet.Port.IntValue()
		if pr.HTTPGet.Port.Type == intstr.String {
			p, ok := names[pr.HTTPGet.Port.StrVal]
			if !ok {
				r.fail("%s: %s probe port %q is not a named containerPort", pc.id(), typ, pr.HTTPGet.Port.StrVal)
				continue
			}
			port = p
		}
		l, ok := byPort[port]
		if !ok {
			r.fail("%s: %s probe targets port %d where nothing listens", pc.id(), typ, port)
			continue
		}
		allowed := allowedProbePaths[l.kind][typ]
		if !slices.Contains(allowed, pr.HTTPGet.Path) {
			r.fail("%s: %s probe path %q on the %s listener (%s); allowed: %v", pc.id(), typ, pr.HTTPGet.Path, l.name, l.kind, allowed)
		}
		if pr.HTTPGet.Scheme != "" && pr.HTTPGet.Scheme != corev1.URISchemeHTTP {
			r.fail("%s: %s probe uses %s; the probe listeners serve plain HTTP", pc.id(), typ, pr.HTTPGet.Scheme)
		}
	}
}

// checkCMPSidecars verifies that config management plugin sidecars share the socket
// directory with the repo-server and have their plugin.yaml.
func checkCMPSidecars(r *wiringReport, pcs []parsedContainer, fc fileChecker) {
	var repo *parsedContainer
	for i := range pcs {
		if pcs[i].sub == "repo-server" {
			repo = &pcs[i]
		}
	}
	for _, pc := range pcs {
		if pc.sub != "cmp-server" {
			continue
		}
		if repo == nil {
			r.fail("%s: cmp-server sidecar outside the repo-server pod", pc.id())
			continue
		}
		sock := effectivePath(pc, "socket-dir", cmp.EnvSocketDir, "", cmpwire.DefaultSocketDir)
		repoSock := effectivePath(*repo, "plugin-socket-dir", "", "", cmpwire.DefaultSocketDir)
		m1, _ := mountFor(pc.c, sock)
		m2, _ := mountFor(repo.c, repoSock)
		switch {
		case m1 == nil || m2 == nil:
			r.fail("%s: socket directory %s (repo-server %s) is not a shared volume", pc.id(), sock, repoSock)
		case m1.Name != m2.Name:
			r.fail("%s: socket directory volume %s differs from the repo-server's %s", pc.id(), m1.Name, m2.Name)
		}
		cfgDir := effectivePath(pc, "config-dir-path", "", "", cmp.DefaultConfigDir)
		fc.covered(pc, "plugin.yaml", path.Join(cfgDir, "plugin.yaml"), false, false)
	}
}

// checkCrossPod verifies requirements spanning components: mutual TLS client certificates
// for a repo-server or commit-server that requires them, and the tree API.
func checkCrossPod(r *wiringReport, pcs []parsedContainer) {
	repoMTLS, commitMTLS := false, false
	for _, pc := range pcs {
		switch pc.sub {
		case "repo-server":
			ca := effectivePath(pc, "tls-client-ca", "PATROL_REPO_SERVER_CLIENT_CA_PATH", "", "/app/config/reposerver/mtls/client-ca.crt")
			if m, _ := mountFor(pc.c, ca); m != nil {
				repoMTLS = true
			}
		case "commit-server":
			if pc.fs.Changed("tls-client-ca") {
				commitMTLS = true
			}
		case "controller":
			if pc.fs.Changed("tls-client-ca") {
				r.fail("%s: --tls-client-ca on the tree API: patrol-server's tree API client presents no client certificate (internal/server TreeAPIClientOptions)", pc.id())
			}
		}
	}
	for _, pc := range pcs {
		if repoMTLS && slices.Contains([]string{"server", "controller", "appset-controller", "notifications-controller"}, pc.sub) {
			for _, f := range []string{"repo-server-client-cert-path", "repo-server-client-cert-key-path"} {
				fl := pc.fs.Lookup(f)
				if fl == nil {
					r.fail("%s: repo-server requires client certificates but patrolcd %s has no --%s", pc.id(), pc.sub, f)
					continue
				}
				env := ""
				for e, s := range pc.params {
					if s.Flag == f {
						env = e
					}
				}
				p := effectivePath(pc, f, env, "", fl.DefValue)
				if m, _ := mountFor(pc.c, p); m == nil {
					r.fail("%s: repo-server requires client certificates but %s (%s) is not mounted", pc.id(), p, f)
				}
			}
		}
		if commitMTLS && pc.sub == "controller" && !pc.fs.Changed("commit-server-client-cert-path") {
			r.fail("%s: commit-server requires client certificates but --commit-server-client-cert-path is not set", pc.id())
		}
	}
}

// checkServices verifies that every Service selects pods and targets declared ports, and
// that the headless controller Service of the tree API exists.
func checkServices(r *wiringReport, objs []object, pts []podTemplate) {
	for _, o := range byKind(objs, "Service") {
		var svc corev1.Service
		if err := fromUnstructuredErr(o, &svc); err != nil {
			r.fail("%s: %v", o.id(), err)
			continue
		}
		if len(svc.Spec.Selector) == 0 {
			r.fail("%s: no selector", o.id())
			continue
		}
		sel := labels.SelectorFromSet(svc.Spec.Selector)
		var matched []podTemplate
		for _, pt := range pts {
			if strings.Split(pt.owner, "/")[1] == svc.Namespace && sel.Matches(labels.Set(pt.tmpl.Labels)) {
				matched = append(matched, pt)
			}
		}
		if len(matched) != 1 {
			r.fail("%s: selector %v matches %d pod templates, want 1", o.id(), svc.Spec.Selector, len(matched))
			continue
		}
		pt := matched[0]
		for _, sp := range svc.Spec.Ports {
			if !targetPortExists(pt.tmpl.Spec, sp) {
				r.fail("%s: port %s (%d) targets %s, which %s does not declare", o.id(), sp.Name, sp.Port, sp.TargetPort.String(), pt.owner)
			}
		}
	}
	for _, o := range byKind(objs, "StatefulSet") {
		svcName, _, _ := unstructuredString(o, "spec", "serviceName")
		svcObj := find(objs, "Service", svcName)
		if svcObj == nil {
			r.fail("%s: serviceName %q has no rendered Service", o.id(), svcName)
			continue
		}
		var svc corev1.Service
		if err := fromUnstructuredErr(*svcObj, &svc); err != nil {
			r.fail("%s: %v", svcObj.id(), err)
			continue
		}
		if svc.Spec.ClusterIP != corev1.ClusterIPNone {
			r.fail("%s: governing Service of %s must be headless (tree API routing per replica)", svcObj.id(), o.id())
		}
		if !slices.ContainsFunc(svc.Spec.Ports, func(p corev1.ServicePort) bool { return p.Port == 8090 }) {
			r.fail("%s: headless controller Service lacks the tree API port 8090 (wiring.md)", svcObj.id())
		}
	}
}

func targetPortExists(spec corev1.PodSpec, sp corev1.ServicePort) bool {
	tp := sp.TargetPort
	if tp.Type == intstr.Int && tp.IntVal == 0 {
		tp = intstr.FromInt32(sp.Port)
	}
	for _, c := range spec.Containers {
		for _, p := range c.Ports {
			if (tp.Type == intstr.String && p.Name == tp.StrVal) || (tp.Type == intstr.Int && p.ContainerPort == tp.IntVal) {
				return true
			}
		}
	}
	return false
}

func unstructuredString(o object, fields ...string) (string, bool, error) {
	cur := any(o.u.Object)
	for _, f := range fields {
		m, ok := cur.(map[string]any)
		if !ok {
			return "", false, nil
		}
		cur = m[f]
	}
	s, ok := cur.(string)
	return s, ok, nil
}

// TestWiring parses every patrolcd container of every profile with the real command tree and
// checks environment variables, listeners, probes, Services and mounted files against
// wiring.md and bootstrap.md.
func TestWiring(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	containers, others := 0, 0
	notes := map[string]bool{}
	t.Cleanup(func() {
		t.Logf("wiring: %d patrolcd containers parsed, %d other containers (not patrolcd) skipped", containers, others)
		var ns []string
		for n := range notes {
			ns = append(ns, n)
		}
		sort.Strings(ns)
		t.Logf("informational notes (%d):\n%s", len(ns), joinLines(ns))
	})
	for _, p := range profiles(t) {
		t.Run(p.name, func(t *testing.T) {
			t.Parallel()
			rep := checkWiring(t, renderProfile(t, p))
			for _, pr := range rep.problems {
				t.Error(pr)
			}
			mu.Lock()
			containers += rep.containers
			others += len(rep.others)
			for _, n := range rep.notes {
				notes[p.name+": "+n] = true
			}
			mu.Unlock()
			t.Logf("%s: %d patrolcd containers checked; not parsed: %v", p.name, rep.containers, rep.others)
		})
	}
}

// TestWiringNegativeControls feeds deliberately broken workloads to checkWiring and requires
// each defect to be reported.
func TestWiringNegativeControls(t *testing.T) {
	t.Parallel()
	base := renderProfile(t, profile{name: "default", path: filepath.Join(chartDir(), "ci", "default.yaml")})
	cases := []struct {
		name   string
		mutate func(d map[string]any)
		target string // Deployment/StatefulSet name
		want   string
	}{
		{"unknown-flag", func(d map[string]any) { appendArg(d, "server", "--bogus-flag=1") }, "patrol-server", "unknown flag: --bogus-flag"},
		{"bad-flag-value", func(d map[string]any) { appendArg(d, "server", "--port=http") }, "patrol-server", `invalid argument "http"`},
		{"positional-arg", func(d map[string]any) { appendArg(d, "server", "extra") }, "patrol-server", "positional arguments"},
		{"unknown-env", func(d map[string]any) { addEnv(d, "controller", "PATROL_CONTROLER_REPLICAS", "1") }, "patrol-controller", "unknown variable PATROL_CONTROLER_REPLICAS"},
		{"miswired-env", func(d map[string]any) {
			setEnvKey(d, "repo-server", "PATROL_REPO_SERVER_LOGLEVEL", "reposerver.log.format")
		}, "patrol-repo-server", "carries parameter \"reposerver.log.level\""},
		{"dev-env", func(d map[string]any) { addEnv(d, "server", clusters.EnvFakeInCluster, "true") }, "patrol-server", "must never be set"},
		{"undeclared-port", func(d map[string]any) { appendArg(d, "server", "--metrics-port=9999") }, "patrol-server", "listener metrics on port 9999 is not declared"},
		{"liveness-on-readyz", func(d map[string]any) { setProbePath(d, "controller", "livenessProbe", "/readyz") }, "patrol-controller", `liveness probe path "/readyz"`},
		{"token-not-mounted", func(d map[string]any) { removeMount(d, "notifications-controller", "/app/config/internal-token") }, "patrol-notifications-controller", "internal token /app/config/internal-token/token is not provided"},
		{"token-flag-next-to-bootstrap", func(d map[string]any) {
			appendArg(d, "server", "--internal-token-file=/app/config/internal-token/token")
		}, "patrol-server", "disables the Secret fallback"},
		{"path-arg-not-mounted", func(d map[string]any) { appendArg(d, "repo-server", "--helm-plugins-dir=/opt/helm-plugins") }, "patrol-repo-server", "--helm-plugins-dir /opt/helm-plugins is not provided"},
		{"known-hosts-never-seeded", func(d map[string]any) {
			// The chart ships the known hosts ConfigMap without data; with the bootstrap told
			// to skip it, nothing provides ssh_known_hosts.
			spec := d["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
			for _, c := range spec["initContainers"].([]any) {
				if m := c.(map[string]any); m["name"] == "bootstrap" {
					m["args"] = append(m["args"].([]any), "--skip-ssh-known-hosts")
				}
			}
		}, "patrol-server", `ConfigMap patrol-ssh-known-hosts-cm has no key "ssh_known_hosts"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			objs := make([]object, len(base))
			for i, o := range base {
				objs[i] = object{raw: o.raw, source: o.source, u: o.u.DeepCopy()}
			}
			var target *object
			for i := range objs {
				if (objs[i].u.GetKind() == "Deployment" || objs[i].u.GetKind() == "StatefulSet") && objs[i].u.GetName() == c.target {
					target = &objs[i]
				}
			}
			if target == nil {
				t.Fatalf("no workload %s", c.target)
			}
			c.mutate(target.u.Object)
			rep := checkWiring(t, objs)
			if !slices.ContainsFunc(rep.problems, func(p string) bool { return strings.Contains(p, c.want) }) {
				t.Fatalf("want a problem containing %q, got:\n%s", c.want, joinLines(rep.problems))
			}
		})
	}
	// The unmodified profile has no problems (the controls above are not noise).
	if rep := checkWiring(t, base); len(rep.problems) > 0 {
		t.Fatalf("default profile has problems:\n%s", joinLines(rep.problems))
	}
}

// TestDocumentedEnvIsRead checks that every documented variable of the expectations above is
// read somewhere in the non-test Go code (documentation and code agree).
func TestDocumentedEnvIsRead(t *testing.T) {
	t.Parallel()
	var src strings.Builder
	for _, dir := range []string{"internal", "cmd", "pkg"} {
		err := filepath.WalkDir(filepath.Join(repoRoot(), dir), func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			src.Write(data)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	code := src.String()
	set := map[string]bool{}
	for _, s := range subcommands {
		for _, e := range s.env {
			set[e] = true
		}
	}
	for _, e := range envNeverDeploy {
		set[e] = true
	}
	var names []string
	for e := range set {
		names = append(names, e)
	}
	sort.Strings(names)
	for _, e := range names {
		// Names may be built from a prefix in code (PATROL_K8S_*); require the literal or, for
		// the K8S tuning names, the upstream name the clusters package maps.
		if !strings.Contains(code, `"`+e+`"`) && !strings.Contains(code, strings.Replace(e, "PATROL_", "ARGOCD_", 1)) {
			t.Errorf("documented variable %s is not referenced by any Go source under internal/, cmd/ or pkg/", e)
		}
	}
	t.Logf("%d documented variables checked against the code", len(names))
}

// Helpers mutating unstructured workloads (negative controls).

func containersOf(d map[string]any) []any {
	spec := d["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	return spec["containers"].([]any)
}

func containerNamed(d map[string]any, name string) map[string]any {
	for _, c := range containersOf(d) {
		if m := c.(map[string]any); m["name"] == name {
			return m
		}
	}
	panic("no container " + name) // test helper: the caller names a rendered container
}

func appendArg(d map[string]any, container, arg string) {
	c := containerNamed(d, container)
	c["args"] = append(c["args"].([]any), arg)
}

func addEnv(d map[string]any, container, name, value string) {
	c := containerNamed(d, container)
	env, _ := c["env"].([]any)
	c["env"] = append(env, map[string]any{"name": name, "value": value})
}

func setEnvKey(d map[string]any, container, name, key string) {
	c := containerNamed(d, container)
	for _, e := range c["env"].([]any) {
		m := e.(map[string]any)
		if m["name"] == name {
			m["valueFrom"].(map[string]any)["configMapKeyRef"].(map[string]any)["key"] = key
		}
	}
}

func setProbePath(d map[string]any, container, probe, p string) {
	c := containerNamed(d, container)
	c[probe].(map[string]any)["httpGet"].(map[string]any)["path"] = p
}

func removeMount(d map[string]any, container, mountPath string) {
	c := containerNamed(d, container)
	var keep []any
	for _, m := range c["volumeMounts"].([]any) {
		if m.(map[string]any)["mountPath"] != mountPath {
			keep = append(keep, m)
		}
	}
	c["volumeMounts"] = keep
}

func fromUnstructuredErr(o object, into any) error {
	return runtime.DefaultUnstructuredConverter.FromUnstructuredWithValidation(o.u.Object, into, true)
}
