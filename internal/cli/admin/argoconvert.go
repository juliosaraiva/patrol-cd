package admin

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/project"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
)

// Argo CD names and keys read by the conversion (Argo CD v3.5.5).
const (
	argoKeyPrefix          = "argocd.argoproj.io/"
	argoNotificationsGroup = "notifications.argoproj.io"
	argoNotifiedKey        = "notified.notifications.argoproj.io"
	argoSecretTypeLabel    = "argocd.argoproj.io/secret-type"
	argoPartOfValue        = "argocd"
	argoManagedByKey       = "managed-by"
	argoManagedByValue     = "argocd.argoproj.io"
	argoTrackingIDKey      = "argocd.argoproj.io/tracking-id"
	argoDefaultNamespace   = "argocd"
	argoDefaultTracking    = "annotation"
	labelKeyAppName        = "app.kubernetes.io/name"
)

// argoSettingsNames maps Argo CD settings objects to Patrol CD's (ADR-0003).
var argoSettingsNames = map[string]string{
	"argocd-cm":                   settings.DefaultConfigMapName,
	"argocd-rbac-cm":              settings.DefaultRBACConfigMapName,
	"argocd-cmd-params-cm":        settings.DefaultCmdParamsConfigMapName,
	"argocd-ssh-known-hosts-cm":   settings.DefaultSSHKnownHostsConfigMapName,
	"argocd-tls-certs-cm":         settings.DefaultTLSCertsConfigMapName,
	"argocd-gpg-keys-cm":          settings.DefaultGPGKeysConfigMapName,
	"argocd-notifications-cm":     settings.DefaultNotificationsConfigMapName,
	"argocd-secret":               settings.DefaultSecretName,
	"argocd-notifications-secret": settings.DefaultNotificationsSecretName,
}

// Severity of a conversion finding.
const (
	sevInfo     = "INFO"
	sevWarning  = "WARNING"
	sevBlocking = "BLOCKING"
)

// finding is one message of the conversion report.
type finding struct {
	Severity string
	Object   string
	Message  string
}

// convertedObject is a Patrol object produced from an Argo CD object.
type convertedObject struct {
	Obj    *unstructured.Unstructured
	Source string
	// OwnerOf names the converted object (kind/name) a credentials Secret belongs to.
	OwnerKind, OwnerName string
}

// conversion is the result of converting an Argo CD export.
type conversion struct {
	sourceNamespace string
	keepAutoSync    bool
	objects         []convertedObject
	findings        []finding
	inputCount      int
	trackingMethod  string
	instanceLabel   string
	usedNames       map[string]bool
	nsChecks        []nsCheck
}

// nsCheck is an Application or ApplicationSet outside the control-plane namespace, checked
// against the converted namespace settings once everything is converted.
type nsCheck struct {
	ref, namespace, project string
	appSet                  bool
}

func (c *conversion) add(sev, object, format string, args ...any) {
	c.findings = append(c.findings, finding{Severity: sev, Object: object, Message: fmt.Sprintf(format, args...)})
}

func (c *conversion) blocking() int {
	n := 0
	for _, f := range c.findings {
		if f.Severity == sevBlocking {
			n++
		}
	}
	return n
}

// argoRef names an object of the export ("Kind namespace/name").
func (c *conversion) argoRef(obj *unstructured.Unstructured) string {
	ns := obj.GetNamespace()
	if ns == "" {
		ns = c.sourceNamespace
	}
	return obj.GetKind() + " " + ns + "/" + obj.GetName()
}

// argoKeyToPatrol maps an Argo CD annotation or label key. ok is false when the key must be
// dropped (transient or Argo-specific state).
func argoKeyToPatrol(key string) (string, bool) {
	switch {
	case key == corev1.LastAppliedConfigAnnotation:
		return "", false
	case key == argoKeyPrefix+"refresh", key == argoKeyPrefix+"hydrate", key == argoTrackingIDKey,
		key == "applications.argoproj.io/app-name", key == argoNotifiedKey:
		return "", false
	case strings.HasPrefix(key, argoKeyPrefix):
		return v1alpha1.Prefix + strings.TrimPrefix(key, argoKeyPrefix), true
	case strings.HasPrefix(key, argoNotificationsGroup+"/"):
		return "notifications." + v1alpha1.GroupName + "/" + strings.TrimPrefix(key, argoNotificationsGroup+"/"), true
	case strings.HasSuffix(key, "recipients.argocd-notifications.argoproj.io"):
		return strings.TrimSuffix(key, "argocd-notifications.argoproj.io") + "notifications." + v1alpha1.GroupName, true
	}
	if sub, rest, ok := strings.Cut(key, ".argocd.argoproj.io/"); ok && !strings.Contains(sub, "/") {
		return sub + "." + v1alpha1.GroupName + "/" + rest, true
	}
	return key, true
}

// convertMeta copies labels and annotations with Argo CD keys mapped; it reports dropped
// and mapped keys.
func (c *conversion) convertMeta(ref string, labels, annotations map[string]string) (map[string]string, map[string]string) {
	conv := func(in map[string]string, what string) map[string]string {
		if len(in) == 0 {
			return nil
		}
		out := map[string]string{}
		for _, k := range sortedKeys(in) {
			v := in[k]
			switch {
			case k == argoManagedByKey && v == argoManagedByValue:
				out[v1alpha1.AnnotationKeyManagedBy] = v1alpha1.AnnotationValueManagedByPatrol
				continue
			case k == argoNotifiedKey:
				c.add(sevWarning, ref, "notification delivery state (%s) is not transferred: notifications whose trigger condition is currently true are sent once more by Patrol CD", argoNotifiedKey)
				continue
			case k == argoTrackingIDKey:
				c.add(sevInfo, ref, "%s %s removed (Argo CD tracking of this object); Patrol CD adds its own when the parent Application syncs", what, argoTrackingIDKey)
				continue
			case k == v1alpha1.LabelKeyPartOf && v == argoPartOfValue:
				out[k] = v1alpha1.LabelValuePartOf
				continue
			case k == labelKeyAppName && argoSettingsNames[v] != "":
				out[k] = argoSettingsNames[v]
				continue
			}
			nk, ok := argoKeyToPatrol(k)
			if !ok {
				continue
			}
			if nk != k {
				if strings.HasPrefix(k, argoNotificationsGroup+"/subscribe") || k == argoNotificationsGroup+"/subscriptions" || strings.Contains(k, "recipients.argocd-notifications") {
					c.add(sevWarning, ref, "notification subscription %s mapped to %s: Patrol CD delivers it once the Application is imported; disable the Argo CD subscription or notifications controller to avoid duplicate notifications", k, nk)
				}
			}
			out[nk] = v
		}
		if len(out) == 0 {
			return nil
		}
		return out
	}
	return conv(labels, "label"), conv(annotations, "annotation")
}

// strictDecode decodes JSON-like v into out, rejecting fields out does not have.
func strictDecode(v any, out any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	return dec.Decode(out)
}

// newPatrolObject returns an object of a Patrol kind with converted metadata.
func newPatrolObject(kind, name, namespace string, labels, annotations map[string]string, finalizers []string, spec any) (*unstructured.Unstructured, error) {
	m := map[string]any{"apiVersion": v1alpha1.GroupVersion.String(), "kind": kind}
	obj := &unstructured.Unstructured{Object: m}
	obj.SetName(name)
	if namespace != "" {
		obj.SetNamespace(namespace)
	}
	obj.SetLabels(labels)
	obj.SetAnnotations(annotations)
	if len(finalizers) > 0 {
		obj.SetFinalizers(finalizers)
	}
	if spec != nil {
		sm, err := toMap(spec)
		if err != nil {
			return nil, err
		}
		obj.Object["spec"] = sm
	}
	return obj, nil
}

// targetNamespace is the namespace a converted object keeps: objects of the Argo CD
// namespace go to the import namespace (none in the output); Applications and
// ApplicationSets of other namespaces keep theirs.
func (c *conversion) targetNamespace(obj *unstructured.Unstructured) string {
	if ns := obj.GetNamespace(); ns != "" && ns != c.sourceNamespace {
		return ns
	}
	return ""
}

// convertArgoExport converts the objects of an Argo CD export.
func convertArgoExport(objs []*unstructured.Unstructured, sourceNamespace string, keepAutoSync bool) *conversion {
	c := &conversion{sourceNamespace: sourceNamespace, keepAutoSync: keepAutoSync, inputCount: len(objs),
		trackingMethod: argoDefaultTracking, instanceLabel: v1alpha1.LabelKeyAppInstanceDefault, usedNames: map[string]bool{}}
	for _, obj := range objs {
		if obj.GetKind() == "Secret" && objectGroup(obj) == "" {
			c.usedNames["Secret/"+obj.GetName()] = true
		}
		if obj.GetKind() == "ConfigMap" && obj.GetName() == "argocd-cm" {
			data, _, _ := unstructured.NestedStringMap(obj.Object, "data")
			if v := data["application.resourceTrackingMethod"]; v != "" {
				c.trackingMethod = v
			}
			if v := data["application.instanceLabelKey"]; v != "" {
				c.instanceLabel = v
			}
		}
	}
	hasApps := false
	for _, obj := range objs {
		g, kind := objectGroup(obj), obj.GetKind()
		ref := c.argoRef(obj)
		switch {
		case g == "" && kind == "ConfigMap":
			c.convertConfigMap(obj)
		case g == "" && kind == "Secret":
			c.convertSecret(obj)
		case g == argoGroup && kind == "AppProject":
			c.convertProject(obj)
		case g == argoGroup && kind == "Application":
			hasApps = true
			c.convertApplication(obj)
		case g == argoGroup && kind == "ApplicationSet":
			c.convertApplicationSet(obj)
		default:
			c.add(sevBlocking, ref, "unsupported object %s/%s: an Argo CD export contains only ConfigMaps, Secrets, AppProjects, Applications and ApplicationSets", g, kind)
		}
	}
	if hasApps {
		c.trackingFindings()
	}
	c.checkNamespaces()
	c.validateSettings()
	return c
}

// trackingFindings explains how resource tracking moves from Argo CD to Patrol CD.
func (c *conversion) trackingFindings() {
	const procedure = "Transfer procedure: (1) stop Argo CD from changing these resources (disable automated sync of the Argo CD Applications, or scale the argocd-application-controller to 0); " +
		"(2) import into Patrol CD with automated sync disabled (the default of this command); " +
		"(3) sync each Application in Patrol CD: it applies the manifests and adds its own tracking; " +
		"(4) delete the Argo CD Applications without cascading: remove their resources-finalizer.argocd.argoproj.io finalizer first (kubectl -n argocd patch application <name> --type merge -p '{\"metadata\":{\"finalizers\":null}}'), then delete them; " +
		"(5) enable automated sync in Patrol CD (spec.syncPolicy.automated.enabled: true)."
	switch c.trackingMethod {
	case "label":
		c.add(sevWarning, "resource tracking", "Argo CD tracks resources with the label %s (application.resourceTrackingMethod: label); Patrol CD with label tracking uses the same label, so the imported Applications immediately treat the resources Argo CD deployed as their own: if both controllers run they overwrite each other, and pruning in either removes the other's resources. %s", c.instanceLabel, procedure)
	default:
		c.add(sevWarning, "resource tracking", "resources deployed by Argo CD carry %s (application.resourceTrackingMethod: %s); Patrol CD tracks resources with %s and does not recognize the Argo CD annotation, so the imported Applications show those resources as missing until synced, and Patrol CD never prunes or deletes resources it does not track. Ownership of live resources is never transferred automatically. %s",
			argoTrackingIDKey, c.trackingMethod, v1alpha1.AnnotationKeyTrackingID, procedure)
	}
}

// --- settings ------------------------------------------------------------------------

// argoCustomizationKinds are the Argo CD kinds whose customization keys move to Patrol's
// group (other argoproj.io kinds, such as Argo Rollouts, keep theirs).
var argoCustomizationKinds = []string{"Application", "ApplicationSet", "AppProject"}

// renameCustomizationKey maps resource.customizations.<x>.argoproj.io_<Kind> keys.
func renameCustomizationKey(key string) string {
	for _, k := range argoCustomizationKinds {
		if suffix := argoGroup + "_" + k; strings.HasPrefix(key, "resource.customizations.") && strings.HasSuffix(key, "."+suffix) {
			return strings.TrimSuffix(key, suffix) + v1alpha1.GroupName + "_" + k
		}
	}
	return key
}

// knownAddressRenames maps Argo CD service addresses in cmd-params values.
var knownAddressRenames = map[string]string{
	"argocd-repo-server":   "patrol-repo-server",
	"argocd-commit-server": "patrol-commit-server",
	"argocd-dex-server":    "patrol-dex-server",
}

func (c *conversion) convertConfigMap(obj *unstructured.Unstructured) {
	ref := c.argoRef(obj)
	name, ok := argoSettingsNames[obj.GetName()]
	if !ok || !strings.HasSuffix(obj.GetName(), "-cm") {
		c.add(sevWarning, ref, "not an Argo CD settings ConfigMap; skipped")
		return
	}
	data, _, _ := unstructured.NestedStringMap(obj.Object, "data")
	out := map[string]string{}
	for _, k := range sortedKeys(data) {
		v := data[k]
		nk := k
		switch obj.GetName() {
		case "argocd-cm":
			nk = renameCustomizationKey(k)
			if nk != k {
				c.add(sevInfo, ref, "key %s renamed to %s", k, nk)
			}
			switch {
			case k == "resource.customizations":
				v = c.renameLegacyCustomizations(ref, v)
			case k == "url":
				c.add(sevWarning, ref, "url is %q: set it to the external URL of Patrol CD (SSO redirects, notification links)", v)
			case (k == "resource.exclusions" || k == "resource.inclusions") && strings.Contains(v, argoGroup):
				c.add(sevWarning, ref, "%s mentions %s: review it (Patrol CD kinds are in group %s)", k, argoGroup, v1alpha1.GroupName)
			case strings.Contains(v, argoKeyPrefix) || strings.Contains(v, argoGroup+"/v1alpha1"):
				c.add(sevWarning, ref, "%s mentions Argo CD keys or API versions; review it for Patrol CD (%s)", nk, v1alpha1.GroupName)
			}
		case "argocd-cmd-params-cm":
			if k == "hydrator.enabled" && strings.EqualFold(strings.TrimSpace(v), "true") {
				c.add(sevWarning, ref, "hydrator.enabled is true: Patrol CD's source hydrator commits and pushes hydrated manifests for every Application with spec.sourceHydrator as soon as it is imported; stop Argo CD's hydrator first (see the spec.sourceHydrator findings)")
			}
			nv := v
			for from, to := range knownAddressRenames {
				nv = strings.ReplaceAll(nv, from, to)
			}
			if nv != v {
				c.add(sevInfo, ref, "%s: %q changed to %q", k, v, nv)
				v = nv
			}
		case "argocd-notifications-cm":
			if strings.Contains(v, argoNotificationsGroup) || strings.Contains(v, argoKeyPrefix) {
				c.add(sevWarning, ref, "%s mentions Argo CD keys; review it for Patrol CD keys (notifications.%s, %s)", k, v1alpha1.GroupName, v1alpha1.Prefix)
			}
		}
		out[nk] = v
	}
	labels, annotations := c.convertMeta(ref, obj.GetLabels(), obj.GetAnnotations())
	if labels == nil {
		labels = map[string]string{}
	}
	labels[v1alpha1.LabelKeyPartOf] = v1alpha1.LabelValuePartOf
	cm := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "ConfigMap"}}
	cm.SetName(name)
	cm.SetLabels(labels)
	cm.SetAnnotations(annotations)
	if len(out) > 0 {
		m := map[string]any{}
		for k, v := range out {
			m[k] = v
		}
		cm.Object["data"] = m
	}
	c.objects = append(c.objects, convertedObject{Obj: cm, Source: ref})
}

// renameLegacyCustomizations renames argoproj.io/<Kind> keys of the legacy
// resource.customizations YAML.
func (c *conversion) renameLegacyCustomizations(ref, v string) string {
	var m map[string]any
	if err := yaml.Unmarshal([]byte(v), &m); err != nil {
		c.add(sevWarning, ref, "resource.customizations is not valid YAML; copied unchanged: %v", err)
		return v
	}
	changed := false
	for _, k := range argoCustomizationKinds {
		if val, ok := m[argoGroup+"/"+k]; ok {
			delete(m, argoGroup+"/"+k)
			m[v1alpha1.GroupName+"/"+k] = val
			changed = true
		}
	}
	if !changed {
		return v
	}
	out, err := yaml.Marshal(m)
	if err != nil {
		return v
	}
	c.add(sevInfo, ref, "resource.customizations: Argo CD kinds renamed to %s", v1alpha1.GroupName)
	return string(out)
}

// validateSettings runs the Patrol settings parser on the converted settings objects.
func (c *conversion) validateSettings() {
	in := settings.Input{IgnorePartOfLabel: true}
	found := false
	for _, co := range c.objects {
		obj := co.Obj
		switch {
		case obj.GetKind() == "ConfigMap":
			cm := &corev1.ConfigMap{}
			if runtimeConvert(obj.Object, cm) == nil {
				in.ConfigMaps = append(in.ConfigMaps, cm)
				found = true
			}
		case obj.GetKind() == "Secret" && (obj.GetName() == settings.DefaultSecretName || obj.GetName() == settings.DefaultNotificationsSecretName):
			sec := &corev1.Secret{}
			if runtimeConvert(obj.Object, sec) == nil {
				in.Secrets = append(in.Secrets, sec)
			}
		}
	}
	if !found {
		return
	}
	for _, is := range settings.Parse(in).Validate() {
		if is.Key == settings.SecretKeyServerSecretKey {
			continue // generated by patrol-server at its first start
		}
		if is.Source == settings.SourceSecret && strings.Contains(is.Message, "not found") {
			continue
		}
		c.add(sevWarning, "settings", "%s", is.String())
	}
}

// --- Secrets -----------------------------------------------------------------------------

// secretData returns the decoded data (and stringData) of a Secret.
func secretData(obj *unstructured.Unstructured) (map[string][]byte, error) {
	out := map[string][]byte{}
	data, _, _ := unstructured.NestedStringMap(obj.Object, "data")
	for k, v := range data {
		b, err := base64.StdEncoding.DecodeString(v)
		if err != nil {
			return nil, fmt.Errorf("key %s: invalid base64", k)
		}
		out[k] = b
	}
	sdata, _, _ := unstructured.NestedStringMap(obj.Object, "stringData")
	for k, v := range sdata {
		out[k] = []byte(v)
	}
	return out, nil
}

func newSecretObject(name, namespace string, labels, annotations map[string]string, data map[string][]byte) *unstructured.Unstructured {
	sec := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Secret", "type": string(corev1.SecretTypeOpaque)}}
	sec.SetName(name)
	if namespace != "" {
		sec.SetNamespace(namespace)
	}
	sec.SetLabels(labels)
	sec.SetAnnotations(annotations)
	if len(data) > 0 {
		m := map[string]any{}
		for k, v := range data {
			m[k] = base64.StdEncoding.EncodeToString(v)
		}
		sec.Object["data"] = m
	}
	return sec
}

// credsSecretName returns a free name for the credentials Secret of name.
func (c *conversion) credsSecretName(name string) string {
	candidate := name + "-creds"
	for i := 2; c.usedNames["Secret/"+candidate]; i++ {
		candidate = fmt.Sprintf("%s-creds-%d", name, i)
	}
	c.usedNames["Secret/"+candidate] = true
	return candidate
}

func (c *conversion) convertSecret(obj *unstructured.Unstructured) {
	ref := c.argoRef(obj)
	data, err := secretData(obj)
	if err != nil {
		c.add(sevBlocking, ref, "%v", err)
		return
	}
	switch typ := obj.GetLabels()[argoSecretTypeLabel]; typ {
	case "repository", "repository-write", "repo-creds", "repo-write-creds":
		c.convertRepoSecret(obj, ref, typ, data)
		return
	case "cluster":
		c.convertClusterSecret(obj, ref, data)
		return
	case "":
	default:
		c.add(sevBlocking, ref, "unknown %s %q", argoSecretTypeLabel, typ)
		return
	}
	labels, annotations := c.convertMeta(ref, obj.GetLabels(), obj.GetAnnotations())
	name := obj.GetName()
	switch name {
	case "argocd-secret":
		name = settings.DefaultSecretName
		if _, ok := data["server.secretkey"]; ok {
			delete(data, "server.secretkey")
			c.add(sevWarning, ref, "server.secretkey is not copied: patrol-server generates its own signing key, so Argo CD sessions and API tokens are not valid in Patrol CD; users log in again and API tokens are re-issued")
		}
		for _, k := range sortedKeys(data) {
			if strings.HasPrefix(k, "accounts.") && strings.HasSuffix(k, ".tokens") {
				delete(data, k)
				c.add(sevWarning, ref, "%s (API token records) is not copied: tokens signed by Argo CD are not valid in Patrol CD; re-issue them with 'patrol account generate-token'", k)
			}
		}
		if _, ok := data["admin.password"]; ok {
			c.add(sevInfo, ref, "admin.password (bcrypt hash) is copied: the admin password stays the same")
		}
	case "argocd-notifications-secret":
		name = settings.DefaultNotificationsSecretName
	default:
		if obj.GetLabels()[v1alpha1.LabelKeyPartOf] == argoPartOfValue {
			c.add(sevInfo, ref, "labelled %s=%s (a settings reference target): relabelled %s=%s", v1alpha1.LabelKeyPartOf, argoPartOfValue, v1alpha1.LabelKeyPartOf, v1alpha1.LabelValuePartOf)
		}
	}
	if (name == settings.DefaultSecretName || name == settings.DefaultNotificationsSecretName) && labels == nil {
		labels = map[string]string{}
	}
	if name == settings.DefaultSecretName {
		labels[v1alpha1.LabelKeyPartOf] = v1alpha1.LabelValuePartOf
	}
	sec := newSecretObject(name, "", labels, annotations, data)
	c.objects = append(c.objects, convertedObject{Obj: sec, Source: ref})
}

// secretFields reads Argo CD secret keys with type checks.
type secretFields struct {
	data map[string][]byte
	used map[string]bool
	errs []string
}

func (s *secretFields) str(key string) string {
	s.used[key] = true
	return string(s.data[key])
}

func (s *secretFields) boolean(key string) bool {
	s.used[key] = true
	v, ok := s.data[key]
	if !ok || len(v) == 0 {
		return false
	}
	b, err := strconv.ParseBool(string(v))
	if err != nil {
		s.errs = append(s.errs, fmt.Sprintf("%s: invalid boolean %q", key, v))
	}
	return b
}

func (s *secretFields) integer(key string) int64 {
	s.used[key] = true
	v, ok := s.data[key]
	if !ok || len(v) == 0 {
		return 0
	}
	n, err := strconv.ParseInt(string(v), 10, 64)
	if err != nil {
		s.errs = append(s.errs, fmt.Sprintf("%s: invalid integer %q", key, v))
	}
	return n
}

// credential moves a credential key into the credentials Secret data.
func (s *secretFields) credential(into map[string][]byte, key, patrolKey string) {
	s.used[key] = true
	if v := s.data[key]; len(v) > 0 {
		into[patrolKey] = v
	}
}

func (s *secretFields) unused() []string {
	var out []string
	for k := range s.data {
		if !s.used[k] {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}

// repoCredentialKeys are the credential keys of repository Secrets (same names in Patrol).
var repoCredentialKeys = []string{
	v1alpha1.RepositorySecretKeyUsername, v1alpha1.RepositorySecretKeyPassword, v1alpha1.RepositorySecretKeyBearerToken,
	v1alpha1.RepositorySecretKeySSHPrivateKey, v1alpha1.RepositorySecretKeyTLSClientCertData, v1alpha1.RepositorySecretKeyTLSClientCertKey,
	v1alpha1.RepositorySecretKeyGitHubAppPrivateKey, v1alpha1.RepositorySecretKeyGCPServiceAccountKey, v1alpha1.RepositorySecretKeyAzureServicePrincipalClientSecret,
}

// stripArgoSecretMeta removes the Argo CD secret-type label (the kind replaces it).
func stripArgoSecretMeta(labels map[string]string) map[string]string {
	out := maps.Clone(labels)
	delete(out, argoSecretTypeLabel)
	if len(out) == 0 {
		return nil
	}
	return out
}

func (c *conversion) convertRepoSecret(obj *unstructured.Unstructured, ref, typ string, data map[string][]byte) {
	s := &secretFields{data: data, used: map[string]bool{}}
	url := s.str("url")
	if url == "" {
		c.add(sevBlocking, ref, "repository Secret without url")
		return
	}
	repoType := v1alpha1.RepositoryType(s.str("type"))
	switch repoType {
	case "":
		repoType = v1alpha1.RepositoryTypeGit
	case v1alpha1.RepositoryTypeGit, v1alpha1.RepositoryTypeHelm, v1alpha1.RepositoryTypeOCI:
	default:
		c.add(sevBlocking, ref, "unsupported repository type %q", repoType)
		return
	}
	purpose := v1alpha1.RepositoryPurposeRead
	if strings.HasSuffix(typ, "-write") || typ == "repo-write-creds" {
		purpose = v1alpha1.RepositoryPurposeWrite
	}
	access := v1alpha1.RepositoryAccess{
		EnableOCI:                s.boolean("enableOCI"),
		InsecureOCIForceHTTP:     s.boolean("insecureOCIForceHttp"),
		ForceHTTPBasicAuth:       s.boolean("forceHttpBasicAuth"),
		Proxy:                    s.str("proxy"),
		NoProxy:                  s.str("noProxy"),
		UseAzureWorkloadIdentity: s.boolean("useAzureWorkloadIdentity"),
	}
	if appID := s.integer("githubAppID"); appID != 0 {
		access.GitHubApp = &v1alpha1.RepositoryGitHubApp{AppID: appID, EnterpriseBaseURL: s.str("githubAppEnterpriseBaseUrl")}
		if inst := s.integer("githubAppInstallationID"); inst != 0 {
			access.GitHubApp.InstallationID = &inst
		}
	} else {
		s.str("githubAppInstallationID")
		s.str("githubAppEnterpriseBaseUrl")
	}
	clientID := s.str("azureServicePrincipalClientId") + s.str("azureServicePrincipalClientID")
	tenantID := s.str("azureServicePrincipalTenantId") + s.str("azureServicePrincipalTenantID")
	if clientID != "" || tenantID != "" {
		access.AzureServicePrincipal = &v1alpha1.AzureServicePrincipal{ClientID: clientID, TenantID: tenantID, ActiveDirectoryEndpoint: s.str("azureActiveDirectoryEndpoint")}
	} else {
		s.str("azureActiveDirectoryEndpoint")
	}
	creds := map[string][]byte{}
	for _, k := range repoCredentialKeys {
		s.credential(creds, k, k)
	}
	isTemplate := strings.HasPrefix(typ, "repo-")
	kind := kindRepository.Kind
	var spec any
	if isTemplate {
		kind = kindRepositoryCredential.Kind
		spec = v1alpha1.RepositoryCredentialSpec{URLPrefix: url, Type: repoType, Purpose: purpose, RepositoryAccess: access}
	} else {
		rs := v1alpha1.RepositorySpec{
			URL: url, Type: repoType, Name: s.str("name"), Project: s.str("project"), Purpose: purpose,
			Insecure: s.boolean("insecure"), InsecureIgnoreHostKey: s.boolean("insecureIgnoreHostKey"), EnableLFS: s.boolean("enableLfs"), //nolint:staticcheck // deprecated field kept for parity
			Depth: s.integer("depth"), WebhookManifestCacheWarmDisabled: s.boolean("webhookManifestCacheWarmDisabled"), RepositoryAccess: access,
		}
		spec = rs
	}
	if isTemplate {
		// Keys that only apply to repositories are ignored in templates, as upstream.
		for _, k := range []string{"name", "project", "insecure", "insecureIgnoreHostKey", "enableLfs", "depth", "webhookManifestCacheWarmDisabled"} {
			s.used[k] = true
		}
	}
	if len(s.errs) > 0 {
		c.add(sevBlocking, ref, "invalid repository Secret: %s", strings.Join(s.errs, "; "))
		return
	}
	for _, k := range s.unused() {
		c.add(sevWarning, ref, "key %s is not used by Patrol CD; ignored", k)
	}
	labels, annotations := c.convertMeta(ref, stripArgoSecretMeta(obj.GetLabels()), obj.GetAnnotations())
	name := obj.GetName()
	var credsName string
	if len(creds) > 0 {
		credsName = c.credsSecretName(name)
		if isTemplate {
			cs := spec.(v1alpha1.RepositoryCredentialSpec)
			cs.CredentialsRef = &v1alpha1.LocalSecretReference{Name: credsName}
			spec = cs
		} else {
			rs := spec.(v1alpha1.RepositorySpec)
			rs.CredentialsRef = &v1alpha1.LocalSecretReference{Name: credsName}
			spec = rs
		}
	}
	out, err := newPatrolObject(kind, name, "", labels, annotations, nil, spec)
	if err != nil {
		c.add(sevBlocking, ref, "%v", err)
		return
	}
	c.objects = append(c.objects, convertedObject{Obj: out, Source: ref})
	if credsName != "" {
		secretType := map[string]string{
			"repository": v1alpha1.LabelValueSecretTypeRepository, "repository-write": v1alpha1.LabelValueSecretTypeRepositoryWrite,
			"repo-creds": v1alpha1.LabelValueSecretTypeRepoCreds, "repo-write-creds": v1alpha1.LabelValueSecretTypeRepoCredsWrite,
		}[typ]
		sec := newSecretObject(credsName, "", map[string]string{v1alpha1.LabelKeySecretType: secretType}, nil, creds)
		c.objects = append(c.objects, convertedObject{Obj: sec, Source: ref, OwnerKind: kind, OwnerName: name})
	}
}

// argoClusterConfig is the "config" key of an Argo CD cluster Secret.
type argoClusterConfig struct {
	Username        string `json:"username,omitempty"`
	Password        string `json:"password,omitempty"`
	BearerToken     string `json:"bearerToken,omitempty"`
	TLSClientConfig struct {
		Insecure   bool   `json:"insecure,omitempty"`
		ServerName string `json:"serverName,omitempty"`
		CertData   []byte `json:"certData,omitempty"`
		KeyData    []byte `json:"keyData,omitempty"`
		CAData     []byte `json:"caData,omitempty"`
	} `json:"tlsClientConfig"`
	AWSAuthConfig      *v1alpha1.AWSAuthConfig `json:"awsAuthConfig,omitempty"`
	ExecProviderConfig *struct {
		Command     string            `json:"command,omitempty"`
		Args        []string          `json:"args,omitempty"`
		Env         map[string]string `json:"env,omitempty"`
		APIVersion  string            `json:"apiVersion,omitempty"`
		InstallHint string            `json:"installHint,omitempty"`
	} `json:"execProviderConfig,omitempty"`
	ProxyURL           string `json:"proxyUrl,omitempty"`
	DisableCompression bool   `json:"disableCompression,omitempty"`
}

func (c *conversion) convertClusterSecret(obj *unstructured.Unstructured, ref string, data map[string][]byte) {
	s := &secretFields{data: data, used: map[string]bool{}}
	server := s.str("server")
	if server == "" {
		c.add(sevBlocking, ref, "cluster Secret without server")
		return
	}
	name := s.str("name")
	if name == "" {
		name = server
		c.add(sevInfo, ref, "cluster without name: spec.name set to the server URL")
	}
	spec := v1alpha1.ClusterSpec{Server: server, Name: name, Project: s.str("project"), ClusterResources: s.boolean("clusterResources")}
	if ns := s.str("namespaces"); ns != "" {
		spec.Namespaces = splitNamespaces(ns)
	}
	if shard := s.str("shard"); shard != "" {
		n, err := strconv.ParseInt(shard, 10, 64)
		if err != nil || n < 0 {
			s.errs = append(s.errs, fmt.Sprintf("shard: invalid shard %q", shard))
		} else {
			spec.Shard = &n
		}
	}
	creds := map[string][]byte{}
	if raw := s.str("config"); raw != "" {
		var cfg argoClusterConfig
		dec := json.NewDecoder(strings.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&cfg); err != nil {
			var loose argoClusterConfig
			if lerr := json.Unmarshal([]byte(raw), &loose); lerr != nil {
				c.add(sevBlocking, ref, "config is not valid JSON (contents not shown)")
				return
			}
			c.add(sevWarning, ref, "config has fields Patrol CD does not use; ignored (%s)", strings.TrimPrefix(err.Error(), "json: "))
			cfg = loose
		}
		spec.Config.TLSClientConfig = v1alpha1.TLSClientConfig{Insecure: cfg.TLSClientConfig.Insecure, ServerName: cfg.TLSClientConfig.ServerName}
		spec.Config.ProxyURL = cfg.ProxyURL
		spec.Config.DisableCompression = cfg.DisableCompression
		spec.Config.AWSAuthConfig = cfg.AWSAuthConfig
		if e := cfg.ExecProviderConfig; e != nil {
			spec.Config.ExecProviderConfig = &v1alpha1.ExecProviderConfig{Command: e.Command, Args: e.Args, Env: e.Env, APIVersion: e.APIVersion, InstallHint: e.InstallHint}
			if len(e.Env) > 0 {
				c.add(sevWarning, ref, "execProviderConfig.env is copied into the Cluster spec, which is not a Secret: move credentials passed through it to the credentials Secret or to the exec plugin's own configuration")
			}
		}
		put := func(k string, v []byte) {
			if len(v) > 0 {
				creds[k] = v
			}
		}
		put(v1alpha1.ClusterSecretKeyBearerToken, []byte(cfg.BearerToken))
		put(v1alpha1.ClusterSecretKeyUsername, []byte(cfg.Username))
		put(v1alpha1.ClusterSecretKeyPassword, []byte(cfg.Password))
		put(v1alpha1.ClusterSecretKeyCAData, cfg.TLSClientConfig.CAData)
		put(v1alpha1.ClusterSecretKeyCertData, cfg.TLSClientConfig.CertData)
		put(v1alpha1.ClusterSecretKeyKeyData, cfg.TLSClientConfig.KeyData)
	}
	if len(s.errs) > 0 {
		c.add(sevBlocking, ref, "invalid cluster Secret: %s", strings.Join(s.errs, "; "))
		return
	}
	for _, k := range s.unused() {
		c.add(sevWarning, ref, "key %s is not used by Patrol CD; ignored", k)
	}
	labels, annotations := c.convertMeta(ref, stripArgoSecretMeta(obj.GetLabels()), obj.GetAnnotations())
	objName := obj.GetName()
	var credsName string
	if len(creds) > 0 {
		credsName = c.credsSecretName(objName)
		spec.Config.CredentialsRef = &v1alpha1.LocalSecretReference{Name: credsName}
	}
	out, err := newPatrolObject(kindCluster.Kind, objName, "", labels, annotations, nil, spec)
	if err != nil {
		c.add(sevBlocking, ref, "%v", err)
		return
	}
	c.objects = append(c.objects, convertedObject{Obj: out, Source: ref})
	if credsName != "" {
		sec := newSecretObject(credsName, "", map[string]string{v1alpha1.LabelKeySecretType: v1alpha1.LabelValueSecretTypeCluster}, nil, creds)
		c.objects = append(c.objects, convertedObject{Obj: sec, Source: ref, OwnerKind: kindCluster.Kind, OwnerName: objName})
	}
}

// --- Patrol kinds ------------------------------------------------------------------------

// argoFinalizerEquivalent returns the Patrol CD finalizer equivalent to an Argo CD
// finalizer ("" when the finalizer is not an Argo CD one).
func argoFinalizerEquivalent(f string) string {
	base, variant, _ := strings.Cut(f, "/")
	for _, name := range []string{"resources-finalizer", "pre-delete-finalizer", "post-delete-finalizer"} {
		if base == name+".argocd.argoproj.io" {
			if variant != "" {
				return name + "." + v1alpha1.GroupName + "/" + variant
			}
			return name + "." + v1alpha1.GroupName
		}
	}
	return ""
}

// convertFinalizers drops Argo CD finalizers (never honored by Patrol CD) and keeps others.
func (c *conversion) convertFinalizers(ref string, finalizers []string, applications bool) []string {
	var out []string
	for _, f := range finalizers {
		if eq := argoFinalizerEquivalent(f); eq != "" {
			if applications {
				c.add(sevWarning, ref, "finalizer %s is an Argo CD finalizer and is not honored by Patrol CD; it was removed, so deleting the Patrol CD Application does not delete its resources. After the tracking transfer, add %s if deletion should cascade", f, eq)
			} else {
				c.add(sevInfo, ref, "finalizer %s removed (Argo CD finalizer)", f)
			}
			continue
		}
		c.add(sevWarning, ref, "finalizer %s is kept: it blocks deletion until its controller removes it", f)
		out = append(out, f)
	}
	return out
}

// rewriteArgoGroupValues maps group fields equal to argoproj.io for Argo CD kinds
// (ignoreDifferences, resource allow/deny lists).
func (c *conversion) rewriteArgoGroupValues(ref string, v any) {
	switch t := v.(type) {
	case map[string]any:
		if g, ok := t["group"].(string); ok && g == argoGroup {
			if k, _ := t["kind"].(string); slices.Contains(argoCustomizationKinds, k) || k == "*" || k == "" {
				t["group"] = v1alpha1.GroupName
				c.add(sevInfo, ref, "group %s of kind %q changed to %s", argoGroup, k, v1alpha1.GroupName)
			}
		}
		for _, val := range t {
			c.rewriteArgoGroupValues(ref, val)
		}
	case []any:
		for _, e := range t {
			c.rewriteArgoGroupValues(ref, e)
		}
	}
}

func (c *conversion) convertProject(obj *unstructured.Unstructured) {
	ref := c.argoRef(obj)
	rawSpec, _ := obj.Object["spec"].(map[string]any)
	if rawSpec == nil {
		rawSpec = map[string]any{}
	}
	c.rewriteArgoGroupValues(ref, rawSpec)
	var spec v1alpha1.AppProjectSpec
	if err := strictDecode(rawSpec, &spec); err != nil {
		c.add(sevBlocking, ref, "spec cannot be converted: %v", err)
		return
	}
	for i := range spec.Roles {
		role := &spec.Roles[i]
		if n := len(role.JWTTokens); n > 0 { //nolint:staticcheck // Argo CD token records are read to drop them
			c.add(sevWarning, ref, "role %s has %d JWT token(s) issued by Argo CD: they are not valid in Patrol CD and were removed; re-issue them with 'patrol proj role create-token'", role.Name, n)
			role.JWTTokens = nil //nolint:staticcheck // see above
		}
	}
	proj := &v1alpha1.AppProject{ObjectMeta: metav1.ObjectMeta{Name: obj.GetName()}, Spec: spec}
	if err := project.ValidateProject(proj); err != nil {
		c.add(sevBlocking, ref, "project is not valid for Patrol CD: %v", err)
		return
	}
	labels, annotations := c.convertMeta(ref, obj.GetLabels(), obj.GetAnnotations())
	finalizers := c.convertFinalizers(ref, obj.GetFinalizers(), false)
	out, err := newPatrolObject(kindAppProject.Kind, obj.GetName(), "", labels, annotations, finalizers, spec)
	if err != nil {
		c.add(sevBlocking, ref, "%v", err)
		return
	}
	c.objects = append(c.objects, convertedObject{Obj: out, Source: ref})
}

// disableAutoSync turns automated sync off (keeping its settings) and reports it.
func (c *conversion) disableAutoSync(ref string, spec *v1alpha1.ApplicationSpec, what string) {
	if spec.SyncPolicy == nil || spec.SyncPolicy.Automated == nil {
		return
	}
	a := spec.SyncPolicy.Automated
	if a.Enabled != nil && !*a.Enabled {
		return
	}
	destructive := []string{}
	if a.GetPrune() {
		destructive = append(destructive, "prune")
	}
	if a.GetSelfHeal() {
		destructive = append(destructive, "selfHeal")
	}
	detail := "automated sync"
	if len(destructive) > 0 {
		detail = fmt.Sprintf("automated sync with %s", strings.Join(destructive, " and "))
	}
	if c.keepAutoSync {
		c.add(sevWarning, ref, "%s is kept (--keep-auto-sync): Patrol CD syncs the %s as soon as imported, takes over the resources it renders and, with prune, deletes tracked resources that are no longer rendered", detail, what)
		return
	}
	disabled := false
	a.Enabled = &disabled
	c.add(sevWarning, ref, "%s was disabled (spec.syncPolicy.automated.enabled: false) so that importing never changes live resources; enable it after the tracking transfer", detail)
}

// hydratorFinding warns about spec.sourceHydrator: Patrol CD hydrates an Application that
// has no hydration status as soon as it exists (docs/architecture/components/hydrator.md),
// so importing it pushes to git while Argo CD's hydrator may still be writing the same
// branch. Automated sync settings do not prevent it.
func (c *conversion) hydratorFinding(ref string, h *v1alpha1.SourceHydrator, what string) {
	if h == nil {
		return
	}
	branch := h.SyncSource.TargetBranch
	if h.HydrateTo != nil && h.HydrateTo.TargetBranch != "" {
		branch = h.HydrateTo.TargetBranch
	}
	repo := h.SyncSource.RepoURL
	if repo == "" {
		repo = h.DrySource.RepoURL
	}
	c.add(sevWarning, ref, "spec.sourceHydrator: when the source hydrator is enabled (hydrator.enabled in %s), Patrol CD hydrates each %s as soon as it is created, whatever its sync policy: it renders %s (path %q, revision %q) and commits and pushes to branch %q of %s with the write credentials of that repository. "+
		"Stop Argo CD's hydrator before importing with --dry-run=false (hydrator.enabled: false in argocd-cmd-params-cm and restart the argocd-application-controller, or scale it to 0), or both push to the same branch",
		settings.DefaultCmdParamsConfigMapName, what, h.DrySource.RepoURL, h.DrySource.Path, h.DrySource.TargetRevision, branch, repo)
}

// generatorTemplateKinds are the generator fields whose objects may carry a template.
var generatorTemplateKinds = []string{"list", "clusters", "git", "scmProvider", "clusterDecisionResource", "pullRequest", "plugin", "matrix", "merge"}

// convertGeneratorTemplates converts, in place, the generator-level templates of the raw
// generators list gens of an ApplicationSet (nested matrix and merge children included, at
// any depth). A generator template is merged over spec.template with the generator's fields
// winning (internal/appset/template.MergeTemplate, upstream mergo.Merge), so its automated
// sync, finalizers and Argo CD keys need the same conversion as spec.template.
func (c *conversion) convertGeneratorTemplates(ref, path string, gens any, nested bool) {
	list, _ := gens.([]any)
	for i, g := range list {
		gm, _ := g.(map[string]any)
		for _, kind := range generatorTemplateKinds {
			inner, _ := gm[kind].(map[string]any)
			if inner == nil {
				continue
			}
			p := fmt.Sprintf("%s[%d].%s", path, i, kind)
			if t, ok := inner["template"].(map[string]any); ok {
				c.convertRawTemplate(ref+" "+p+".template", t, nested)
			}
			if kind == "matrix" || kind == "merge" {
				c.convertGeneratorTemplates(ref, p+".generators", inner["generators"], true)
			}
		}
	}
}

// convertRawTemplate converts one raw generator template in place: Argo CD label and
// annotation keys are mapped, Argo CD finalizers dropped, automated sync disabled (unless
// --keep-auto-sync) and spec.sourceHydrator reported. Other fields are left untouched.
func (c *conversion) convertRawTemplate(ref string, t map[string]any, nested bool) {
	var tmpl v1alpha1.ApplicationSetTemplate
	data, err := json.Marshal(t)
	if err == nil {
		err = json.Unmarshal(data, &tmpl)
	}
	if err != nil {
		if nested {
			// Never decoded strictly elsewhere (raw JSON in the CRD); never applied either.
			c.add(sevWarning, ref, "template cannot be read (%v); copied unchanged: review it", err)
		}
		// Top-level generator templates are reported by the decoding of the spec.
		return
	}
	labels, annotations := c.convertMeta(ref, tmpl.Labels, tmpl.Annotations)
	finalizers := c.convertFinalizers(ref, tmpl.Finalizers, true)
	c.disableAutoSync(ref, &tmpl.Spec, "generated Applications")
	c.hydratorFinding(ref, tmpl.Spec.SourceHydrator, "generated Application")

	meta, _ := t["metadata"].(map[string]any)
	if meta == nil && (labels != nil || annotations != nil || len(finalizers) > 0) {
		meta = map[string]any{}
		t["metadata"] = meta
	}
	if meta != nil {
		setRawStringMap(meta, "labels", labels)
		setRawStringMap(meta, "annotations", annotations)
		if len(finalizers) > 0 {
			fs := make([]any, len(finalizers))
			for i, f := range finalizers {
				fs[i] = f
			}
			meta["finalizers"] = fs
		} else {
			delete(meta, "finalizers")
		}
	}
	if sp := tmpl.Spec.SyncPolicy; sp != nil && sp.Automated != nil && sp.Automated.Enabled != nil {
		spec, _ := t["spec"].(map[string]any)
		rawSP, _ := spec["syncPolicy"].(map[string]any)
		if automated, ok := rawSP["automated"].(map[string]any); ok {
			automated["enabled"] = *sp.Automated.Enabled
		}
	}
}

// setRawStringMap sets m[key] to v, or deletes it when v is empty.
func setRawStringMap(m map[string]any, key string, v map[string]string) {
	if len(v) == 0 {
		delete(m, key)
		return
	}
	out := make(map[string]any, len(v))
	for k, val := range v {
		out[k] = val
	}
	m[key] = out
}

func (c *conversion) convertApplication(obj *unstructured.Unstructured) {
	ref := c.argoRef(obj)
	rawSpec, _ := obj.Object["spec"].(map[string]any)
	if rawSpec == nil {
		c.add(sevBlocking, ref, "Application without spec")
		return
	}
	c.rewriteArgoGroupValues(ref, rawSpec)
	var spec v1alpha1.ApplicationSpec
	if err := strictDecode(rawSpec, &spec); err != nil {
		c.add(sevBlocking, ref, "spec cannot be converted: %v", err)
		return
	}
	n := 0
	if spec.Source != nil {
		n++
	}
	if len(spec.Sources) > 0 {
		n++
	}
	if spec.SourceHydrator != nil {
		n++
	}
	if n != 1 {
		c.add(sevBlocking, ref, "exactly one of spec.source, spec.sources or spec.sourceHydrator must be set")
		return
	}
	if (spec.Destination.Server == "") == (spec.Destination.Name == "") {
		c.add(sevBlocking, ref, "exactly one of spec.destination.server or spec.destination.name must be set")
		return
	}
	if _, ok := obj.Object["operation"]; ok {
		c.add(sevWarning, ref, "an in-flight Argo CD operation is not transferred; start a sync in Patrol CD if it is still needed")
	}
	c.disableAutoSync(ref, &spec, "Application")
	c.hydratorFinding(ref, spec.SourceHydrator, "Application")
	ns := c.targetNamespace(obj)
	if ns != "" {
		c.nsChecks = append(c.nsChecks, nsCheck{ref: ref, namespace: ns, project: spec.Project})
	}
	labels, annotations := c.convertMeta(ref, obj.GetLabels(), obj.GetAnnotations())
	finalizers := c.convertFinalizers(ref, obj.GetFinalizers(), true)
	out, err := newPatrolObject(kindApplication.Kind, obj.GetName(), ns, labels, annotations, finalizers, spec)
	if err != nil {
		c.add(sevBlocking, ref, "%v", err)
		return
	}
	c.objects = append(c.objects, convertedObject{Obj: out, Source: ref})
}

func (c *conversion) convertApplicationSet(obj *unstructured.Unstructured) {
	ref := c.argoRef(obj)
	rawSpec, _ := obj.Object["spec"].(map[string]any)
	if rawSpec == nil {
		c.add(sevBlocking, ref, "ApplicationSet without spec")
		return
	}
	c.rewriteArgoGroupValues(ref, rawSpec)
	if n := rewriteSelectorKeys(rawSpec["generators"]); n > 0 {
		c.add(sevInfo, ref, "%d generator label selector key(s) mapped from %s to %s (Patrol CD evaluates cluster selectors against %s=%s)", n, argoKeyPrefix, v1alpha1.Prefix, v1alpha1.LabelKeySecretType, v1alpha1.LabelValueSecretTypeCluster)
	}
	c.convertGeneratorTemplates(ref, "generators", rawSpec["generators"], false)
	var spec v1alpha1.ApplicationSetSpec
	if err := strictDecode(rawSpec, &spec); err != nil {
		c.add(sevBlocking, ref, "spec cannot be converted: %v", err)
		return
	}
	tmplRef := ref + " template"
	tl, ta := c.convertMeta(tmplRef, spec.Template.Labels, spec.Template.Annotations)
	spec.Template.Labels, spec.Template.Annotations = tl, ta
	spec.Template.Finalizers = c.convertFinalizers(tmplRef, spec.Template.Finalizers, true)
	c.disableAutoSync(tmplRef, &spec.Template.Spec, "generated Applications")
	c.hydratorFinding(tmplRef, spec.Template.Spec.SourceHydrator, "generated Application")
	if spec.TemplatePatch != nil && strings.Contains(*spec.TemplatePatch, "automated") {
		c.add(sevWarning, ref, "templatePatch sets syncPolicy.automated: review it, it may re-enable automated sync")
	}
	var secretNames []string
	refs := map[string]bool{}
	gens, _ := rawSpec["generators"].([]any)
	for _, g := range gens {
		secretRefs(g, refs)
	}
	for name := range refs {
		if !c.usedNames["Secret/"+name] {
			secretNames = append(secretNames, name)
		}
	}
	slices.Sort(secretNames)
	for _, name := range secretNames {
		c.add(sevWarning, ref, "a generator references Secret %s, which is not in the export: create it in the Patrol CD namespace", name)
	}
	if data, err := json.Marshal(rawSpec["generators"]); err == nil && bytes.Contains(data, []byte(argoKeyPrefix)) {
		c.add(sevWarning, ref, "a generator still mentions %s keys (in templates or values): review them, Patrol CD objects carry %s keys", argoKeyPrefix, v1alpha1.Prefix)
	}
	c.add(sevInfo, ref, "Applications this ApplicationSet generated in Argo CD are adopted by the Patrol CD ApplicationSet controller when they are imported too")
	ns := c.targetNamespace(obj)
	if ns != "" {
		c.nsChecks = append(c.nsChecks, nsCheck{ref: ref, namespace: ns, appSet: true})
	}
	labels, annotations := c.convertMeta(ref, obj.GetLabels(), obj.GetAnnotations())
	finalizers := c.convertFinalizers(ref, obj.GetFinalizers(), false)
	out, err := newPatrolObject(kindApplicationSet.Kind, obj.GetName(), ns, labels, annotations, finalizers, spec)
	if err != nil {
		c.add(sevBlocking, ref, "%v", err)
		return
	}
	c.objects = append(c.objects, convertedObject{Obj: out, Source: ref})
}

// convertedObject returns the converted object of kind and name, or nil.
func (c *conversion) converted(kind, name string) *unstructured.Unstructured {
	for _, co := range c.objects {
		if co.Obj.GetKind() == kind && co.Obj.GetName() == name && co.OwnerKind == "" {
			return co.Obj
		}
	}
	return nil
}

// checkNamespaces reports Applications and ApplicationSets outside the control-plane
// namespace that the converted settings or projects do not enable.
func (c *conversion) checkNamespaces() {
	var params map[string]string
	if cm := c.converted("ConfigMap", settings.DefaultCmdParamsConfigMapName); cm != nil {
		params, _, _ = unstructured.NestedStringMap(cm.Object, "data")
	}
	permitted := func(patterns []string, ns string) bool {
		return project.IsAppNamespacePermitted(&v1alpha1.AppProject{Spec: v1alpha1.AppProjectSpec{SourceNamespaces: patterns}}, ns, "")
	}
	for _, chk := range c.nsChecks {
		key := cmdParamsAppNamespacesKey
		if chk.appSet {
			key = cmdParamsAppSetNamespacesKey
		}
		if !permitted(splitNamespaces(params[key]), chk.namespace) {
			c.add(sevWarning, chk.ref, "namespace %s is not enabled by %s of %s: add it, or Patrol CD ignores this object", chk.namespace, key, settings.DefaultCmdParamsConfigMapName)
		}
		if chk.appSet || chk.project == "" {
			continue
		}
		proj := c.converted(kindAppProject.Kind, chk.project)
		if proj == nil {
			c.add(sevWarning, chk.ref, "project %s is not in the export: it must exist and list %s in spec.sourceNamespaces", chk.project, chk.namespace)
			continue
		}
		patterns, _, _ := unstructured.NestedStringSlice(proj.Object, "spec", "sourceNamespaces")
		if !permitted(patterns, chk.namespace) {
			c.add(sevWarning, chk.ref, "project %s does not list namespace %s in spec.sourceNamespaces: Patrol CD refuses the Application until it does", chk.project, chk.namespace)
		}
	}
}

// rewriteSelectorKeys maps Argo CD keys in label selectors (matchLabels keys and
// matchExpressions keys) under v; it returns the number of keys mapped.
func rewriteSelectorKeys(v any) int {
	n := 0
	switch t := v.(type) {
	case map[string]any:
		if ml, ok := t["matchLabels"].(map[string]any); ok {
			for _, k := range sortedKeys(ml) {
				if nk, ok := argoKeyToPatrol(k); ok && nk != k {
					ml[nk] = ml[k]
					delete(ml, k)
					n++
				}
			}
		}
		if me, ok := t["matchExpressions"].([]any); ok {
			for _, e := range me {
				if em, ok := e.(map[string]any); ok {
					if k, ok := em["key"].(string); ok {
						if nk, ok := argoKeyToPatrol(k); ok && nk != k {
							em["key"] = nk
							n++
						}
					}
				}
			}
		}
		for _, val := range t {
			n += rewriteSelectorKeys(val)
		}
	case []any:
		for _, e := range t {
			n += rewriteSelectorKeys(e)
		}
	}
	return n
}
