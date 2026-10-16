package admin

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/yaml"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/project"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
)

// Keys of patrol-cmd-params-cm selecting the additional Application and ApplicationSet
// namespaces (upstream application.namespaces, applicationsetcontroller.namespaces).
const (
	cmdParamsAppNamespacesKey    = "application.namespaces"
	cmdParamsAppSetNamespacesKey = "applicationsetcontroller.namespaces"
)

// Secrets that belong to an installation rather than to its configuration; export never
// includes them (upstream: argocd-initial-admin-secret, argocd-redis, TLS Secrets).
const (
	initialAdminSecretName  = "patrol-initial-admin-secret"
	internalTokenSecretName = "patrol-internal-token"
)

// maxBackupBytes bounds an import (or argocd-import) source.
const maxBackupBytes = 256 << 20

// requiredSettingsConfigMaps are exported like upstream's argocd-cm, argocd-rbac-cm,
// argocd-ssh-known-hosts-cm and argocd-tls-certs-cm: a missing one fails the export.
var requiredSettingsConfigMaps = []string{
	settings.DefaultConfigMapName,
	settings.DefaultRBACConfigMapName,
	settings.DefaultSSHKnownHostsConfigMapName,
	settings.DefaultTLSCertsConfigMapName,
}

// optionalSettingsConfigMaps are exported when present (Patrol addition: upstream does not
// back up the GnuPG key and notification ConfigMaps).
var optionalSettingsConfigMaps = []string{
	settings.DefaultGPGKeysConfigMapName,
	settings.DefaultNotificationsConfigMapName,
}

// isSettingsConfigMap reports whether name is a ConfigMap export and import handle.
func isSettingsConfigMap(name string) bool {
	for _, n := range requiredSettingsConfigMaps {
		if n == name {
			return true
		}
	}
	for _, n := range optionalSettingsConfigMaps {
		if n == name {
			return true
		}
	}
	return false
}

// nsScope decides which namespaces Applications and ApplicationSets are exported from and
// imported into (upstream security.IsNamespaceEnabled with the cmd-params globs).
type nsScope struct {
	controlPlane    string
	appPatterns     []string
	appSetPatterns  []string
	namespaceScoped bool
}

func (s nsScope) patterns(k patrolKind) []string {
	if k == kindApplicationSet {
		return s.appSetPatterns
	}
	return s.appPatterns
}

// enabled reports whether objects of k in ns are handled.
func (s nsScope) enabled(k patrolKind, ns string) bool {
	if ns == "" || ns == s.controlPlane {
		return true
	}
	if s.namespaceScoped || !k.AppScoped {
		return false
	}
	return project.IsAppNamespacePermitted(&v1alpha1.AppProject{Spec: v1alpha1.AppProjectSpec{SourceNamespaces: s.patterns(k)}}, ns, s.controlPlane)
}

// listNamespace returns the namespace to list k in: "" (all namespaces) when other
// namespaces are enabled for k.
func (s nsScope) listNamespace(k patrolKind) string {
	if k.AppScoped && !s.namespaceScoped && len(s.patterns(k)) > 0 {
		return metav1.NamespaceAll
	}
	return s.controlPlane
}

// splitNamespaces parses a comma-separated namespace list as upstream does.
func splitNamespaces(v string) []string {
	var out []string
	for _, ns := range strings.Split(v, ",") {
		if ns = strings.TrimSpace(ns); ns != "" {
			out = append(out, ns)
		}
	}
	return out
}

// resolveScope fills the namespace globs not given on the command line from
// patrol-cmd-params-cm. A missing ConfigMap means no additional namespaces (upstream exits
// with an error; the control-plane namespace alone is a safe default).
func resolveScope(ctx context.Context, dyn dynamic.Interface, controlPlane string, appNS, appSetNS []string, namespaceScoped bool, log *slog.Logger) (nsScope, error) {
	s := nsScope{controlPlane: controlPlane, appPatterns: appNS, appSetPatterns: appSetNS, namespaceScoped: namespaceScoped}
	if namespaceScoped {
		if len(appNS) > 0 || len(appSetNS) > 0 {
			return s, errors.New("--namespace-scoped cannot be combined with --application-namespaces or --applicationset-namespaces")
		}
		return s, nil
	}
	if len(appNS) > 0 && len(appSetNS) > 0 {
		return s, nil
	}
	cm, err := dyn.Resource(configMapsGVR).Namespace(controlPlane).Get(ctx, settings.DefaultCmdParamsConfigMapName, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		log.Warn(fmt.Sprintf("ConfigMap %s/%s not found; only the control-plane namespace is used for Applications and ApplicationSets", controlPlane, settings.DefaultCmdParamsConfigMapName))
		return s, nil
	case err != nil:
		return s, fmt.Errorf("error reading ConfigMap %s: %w", settings.DefaultCmdParamsConfigMapName, err)
	}
	data, _, _ := unstructured.NestedStringMap(cm.Object, "data")
	if len(appNS) == 0 {
		s.appPatterns = splitNamespaces(data[cmdParamsAppNamespacesKey])
	}
	if len(appSetNS) == 0 {
		s.appSetPatterns = splitNamespaces(data[cmdParamsAppSetNamespacesKey])
	}
	return s, nil
}

// settingsSecretRef matches "$<secret>:<key>" references of patrol-cm values: a reference
// starts a value or a YAML scalar of an embedded document (dex.config, oidc.config), so the
// "$" must follow the start of a line, whitespace or a quote (not "$x:" inside Lua or jq).
var settingsSecretRef = regexp.MustCompile(`(?m)(?:^|[\s"'])\$([a-z0-9]([-a-z0-9.]*[a-z0-9])?):`)

// settingsSecretRefs returns the Secret names referenced from settings ConfigMap data.
func settingsSecretRefs(data map[string]string, into map[string]bool) {
	for _, v := range data {
		for _, m := range settingsSecretRef.FindAllStringSubmatch(v, -1) {
			into[m[1]] = true
		}
	}
}

// secretRefs collects the names of Secrets referenced from an object's spec: values of
// "name"/"secretName" inside maps stored under keys ending in "Ref" or "ref"
// (credentialsRef, tokenSecretRef, caSecretRef, tokenRef, ...).
func secretRefs(v any, into map[string]bool) {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if strings.HasSuffix(k, "Ref") || strings.HasSuffix(k, "ref") {
				if m, ok := val.(map[string]any); ok {
					for _, field := range []string{"name", "secretName"} {
						if s, ok := m[field].(string); ok && s != "" {
							into[s] = true
						}
					}
				}
			}
			secretRefs(val, into)
		}
	case []any:
		for _, e := range t {
			secretRefs(e, into)
		}
	}
}

// isInstallationSecret reports whether a Secret belongs to the installation rather than to
// its configuration (upstream export leaves out argocd-initial-admin-secret, argocd-redis
// and TLS Secrets): the initial admin password, the internal token, service-account tokens
// and Helm releases. Neither export nor import ever handles them.
func isInstallationSecret(sec *unstructured.Unstructured) bool {
	switch sec.GetName() {
	case initialAdminSecretName, internalTokenSecretName:
		return true
	}
	t, _, _ := unstructured.NestedString(sec.Object, "type")
	return t == string(corev1.SecretTypeServiceAccountToken) || strings.HasPrefix(t, "helm.sh/")
}

// isManagedPatrolSecret reports whether a Secret is managed by Patrol CD (upstream
// isArgoCDSecret): patrol-secret, the notifications Secret, and Secrets labelled with
// cd.patrol.example/secret-type or annotated cd.patrol.example/managed-by=patrol-cd. Only
// these are ever pruned by import.
func isManagedPatrolSecret(sec *unstructured.Unstructured) bool {
	if isInstallationSecret(sec) {
		return false
	}
	switch sec.GetName() {
	case settings.DefaultSecretName, settings.DefaultNotificationsSecretName:
		return true
	}
	if _, ok := sec.GetLabels()[v1alpha1.LabelKeySecretType]; ok {
		return true
	}
	return sec.GetAnnotations()[v1alpha1.AnnotationKeyManagedBy] == v1alpha1.AnnotationValueManagedByPatrol
}

// isPatrolSecret reports whether a Secret is part of the Patrol configuration that export
// writes: the managed Secrets (isManagedPatrolSecret) plus Secrets referenced by Patrol
// objects or by "$<secret>:<key>" settings values (Patrol addition: a backup restores the
// credentials of objects that reference unlabelled Secrets). Referenced-only Secrets are
// restored by import but never pruned: other workloads may share them.
func isPatrolSecret(sec *unstructured.Unstructured, referenced map[string]bool) bool {
	if isInstallationSecret(sec) {
		return false
	}
	return isManagedPatrolSecret(sec) || referenced[sec.GetName()]
}

// isRequiredSettingsSecret reports whether name is a settings Secret of the installation
// (patrol-secret: admin password hash, server signing key, TLS; the notifications Secret).
// Import never prunes them: a backup without them must not lock the administrator out.
func isRequiredSettingsSecret(name string) bool {
	return name == settings.DefaultSecretName || name == settings.DefaultNotificationsSecretName
}

// cleanForBackup strips an object to what a backup restores (upstream export keeps name,
// labels, annotations, finalizers and the body): uid, resourceVersion, generation,
// timestamps, managedFields, the last-applied-configuration annotation (it may embed Secret
// data) and status are removed; owner references are kept (their UIDs are rewritten on
// import). The namespace is kept only outside the control-plane namespace.
func cleanForBackup(obj *unstructured.Unstructured, controlPlane string, keepStatus bool) *unstructured.Unstructured {
	out := obj.DeepCopy()
	name, ns := out.GetName(), out.GetNamespace()
	labels, annotations := out.GetLabels(), out.GetAnnotations()
	finalizers, owners := out.GetFinalizers(), out.GetOwnerReferences()
	delete(annotations, corev1.LastAppliedConfigAnnotation)
	unstructured.RemoveNestedField(out.Object, "metadata")
	if !keepStatus {
		unstructured.RemoveNestedField(out.Object, "status")
	}
	out.SetName(name)
	if ns != controlPlane {
		out.SetNamespace(ns)
	}
	if len(labels) > 0 {
		out.SetLabels(labels)
	}
	if len(annotations) > 0 {
		out.SetAnnotations(annotations)
	}
	if len(finalizers) > 0 {
		out.SetFinalizers(finalizers)
	}
	if len(owners) > 0 {
		for i := range owners {
			owners[i].UID = ""
		}
		out.SetOwnerReferences(owners)
	}
	return out
}

// writeBackupObject writes one object as a YAML document followed by "---".
func writeBackupObject(w io.Writer, obj *unstructured.Unstructured) error {
	data, err := yaml.Marshal(obj.Object)
	if err != nil {
		return fmt.Errorf("error marshaling %s: %w", objectRef(obj), err)
	}
	if _, err := w.Write(data); err != nil {
		return err
	}
	_, err = io.WriteString(w, yamlSeparator)
	return err
}

// splitObjects decodes a YAML (or JSON) stream into objects; empty documents are skipped
// and v1 Lists are expanded. Integers decode as int64 (as from the API server).
func splitObjects(data []byte) ([]*unstructured.Unstructured, error) {
	reader := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(data)))
	var out []*unstructured.Unstructured
	for i := 1; ; i++ {
		doc, err := reader.Read()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return out, nil
			}
			return nil, fmt.Errorf("error reading document %d: %w", i, err)
		}
		if len(bytes.TrimSpace(doc)) == 0 {
			continue
		}
		j, err := yaml.YAMLToJSON(doc)
		if err != nil {
			return nil, fmt.Errorf("error decoding document %d: %w", i, err)
		}
		if t := bytes.TrimSpace(j); len(t) == 0 || bytes.Equal(t, []byte("null")) || bytes.Equal(t, []byte("{}")) {
			continue
		}
		obj, _, err := unstructured.UnstructuredJSONScheme.Decode(j, nil, nil)
		if err != nil {
			return nil, fmt.Errorf("error decoding document %d: %w", i, err)
		}
		switch t := obj.(type) {
		case *unstructured.Unstructured:
			if t.GetAPIVersion() == "" || t.GetKind() == "" {
				return nil, fmt.Errorf("document %d has no apiVersion or kind", i)
			}
			out = append(out, t)
		case *unstructured.UnstructuredList:
			for j := range t.Items {
				out = append(out, t.Items[j].DeepCopy())
			}
		default:
			return nil, fmt.Errorf("document %d: unsupported object %T", i, obj)
		}
	}
}

// readSource reads an import source: "-" is stdin. The size is bounded by maxBackupBytes.
func readSource(path string, stdin io.Reader) ([]byte, error) {
	var r io.Reader
	if path == "-" {
		r = stdin
	} else {
		f, err := openFile(path)
		if err != nil {
			return nil, err
		}
		defer func() { _ = f.Close() }()
		r = f
	}
	data, err := io.ReadAll(io.LimitReader(r, maxBackupBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxBackupBytes {
		return nil, fmt.Errorf("%s exceeds %d bytes", path, maxBackupBytes)
	}
	return data, nil
}
