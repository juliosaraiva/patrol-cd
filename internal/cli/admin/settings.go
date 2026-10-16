package admin

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/juliosaraiva/patrol-cd/internal/settings"
)

// settingsOptions select where "admin settings" reads the settings from.
type settingsOptions struct {
	kube        *kubeOptions
	cmPath      string
	secretPath  string
	files       []string
	loadCluster bool
	// notes are informational messages about the loaded settings.
	notes []string
}

// randomKey returns a random server key for offline validation (never printed).
func randomKey() ([]byte, error) {
	key := make([]byte, settings.ServerSecretKeyLength)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	return key, nil
}

func newSettingsCommand(f *Factory, kube *kubeOptions) *cobra.Command {
	o := &settingsOptions{kube: kube}
	cmd := &cobra.Command{
		Use:   "settings",
		Short: "Provides set of commands for settings validation and troubleshooting",
		RunE:  func(c *cobra.Command, _ []string) error { return c.Help() },
	}
	pf := cmd.PersistentFlags()
	pf.StringVar(&o.cmPath, "patrol-cm-path", "", "Path to local patrol-cm.yaml file")
	pf.StringVar(&o.secretPath, "patrol-secret-path", "", "Path to local patrol-secret.yaml file")
	// Upstream flag names, kept for scripts written for Argo CD.
	pf.StringVar(&o.cmPath, "argocd-cm-path", "", "Alias of --patrol-cm-path")
	pf.StringVar(&o.secretPath, "argocd-secret-path", "", "Alias of --patrol-secret-path")
	_ = pf.MarkHidden("argocd-cm-path")
	_ = pf.MarkHidden("argocd-secret-path")
	pf.StringArrayVar(&o.files, "settings-file", nil, "Additional local settings ConfigMap/Secret manifest (patrol-rbac-cm.yaml, patrol-notifications-cm.yaml, ...); can be repeated")
	pf.BoolVar(&o.loadCluster, "load-cluster-settings", false, "Indicates that config map and secret should be loaded from cluster unless local file path is provided")

	cmd.AddCommand(
		newValidateSettingsCommand(f, o),
		newResourceOverridesCommand(f, o),
		newRBACCommand(f, kube),
	)
	return cmd
}

// manifestObjects decodes the ConfigMaps and Secrets of a local manifest file.
func manifestObjects(path string) ([]*corev1.ConfigMap, []*corev1.Secret, error) {
	data, err := readFileLimited(path)
	if err != nil {
		return nil, nil, err
	}
	objs, err := splitObjects(data)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", path, err)
	}
	var cms []*corev1.ConfigMap
	var secrets []*corev1.Secret
	for _, obj := range objs {
		switch obj.GetKind() {
		case "ConfigMap":
			cm := &corev1.ConfigMap{}
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, cm); err != nil {
				return nil, nil, fmt.Errorf("%s: ConfigMap %s: %w", path, obj.GetName(), err)
			}
			cms = append(cms, cm)
		case "Secret":
			sec := &corev1.Secret{}
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, sec); err != nil {
				return nil, nil, fmt.Errorf("%s: Secret %s: %w", path, obj.GetName(), err)
			}
			mergeStringData(sec)
			secrets = append(secrets, sec)
		}
	}
	return cms, secrets, nil
}

// mergeStringData folds stringData into data, as the API server does on write.
func mergeStringData(sec *corev1.Secret) {
	if len(sec.StringData) == 0 {
		return
	}
	if sec.Data == nil {
		sec.Data = map[string][]byte{}
	}
	for k, v := range sec.StringData {
		sec.Data[k] = []byte(v)
	}
	sec.StringData = nil
}

// pickNamed returns the object of the given name, else the first one (upstream reads the
// file given with --argocd-cm-path as argocd-cm whatever its name).
func pickNamed[T metav1.Object](objs []T, name string) (T, bool) {
	var zero T
	for _, o := range objs {
		if o.GetName() == name {
			return o, true
		}
	}
	if len(objs) > 0 {
		return objs[0], true
	}
	return zero, false
}

// load builds a settings snapshot from local files and/or the cluster (upstream
// createSettingsManager): patrol-cm from --patrol-cm-path, else the cluster with
// --load-cluster-settings; patrol-secret from --patrol-secret-path, else the cluster with
// --load-cluster-settings, else a stub with a random server key.
func (o *settingsOptions) load(ctx context.Context) (*settings.Settings, error) {
	if o.cmPath == "" && !o.loadCluster {
		return nil, errors.New("either --patrol-cm-path must be provided or --load-cluster-settings must be set to true")
	}
	in := settings.Input{}
	byName := map[string]bool{}
	addCM := func(cm *corev1.ConfigMap) {
		if !byName["cm/"+cm.Name] {
			byName["cm/"+cm.Name] = true
			in.ConfigMaps = append(in.ConfigMaps, cm)
		}
	}
	addSecret := func(sec *corev1.Secret) {
		if !byName["secret/"+sec.Name] {
			byName["secret/"+sec.Name] = true
			in.Secrets = append(in.Secrets, sec)
		}
	}
	fromFiles := false
	if o.cmPath != "" {
		cms, secrets, err := manifestObjects(o.cmPath)
		if err != nil {
			return nil, err
		}
		cm, ok := pickNamed(cms, settings.DefaultConfigMapName)
		if !ok {
			return nil, fmt.Errorf("%s contains no ConfigMap", o.cmPath)
		}
		cm = cm.DeepCopy()
		cm.Name = settings.DefaultConfigMapName
		addCM(cm)
		for _, s := range secrets {
			addSecret(s)
		}
		fromFiles = true
	}
	if o.secretPath != "" {
		_, secrets, err := manifestObjects(o.secretPath)
		if err != nil {
			return nil, err
		}
		sec, ok := pickNamed(secrets, settings.DefaultSecretName)
		if !ok {
			return nil, fmt.Errorf("%s contains no Secret", o.secretPath)
		}
		main := sec.DeepCopy()
		main.Name = settings.DefaultSecretName
		if len(main.Data[settings.SecretKeyServerSecretKey]) == 0 {
			// patrol-server generates the key at its first start (settings.EnsureServerSecretKey),
			// so manifests normally omit it.
			key, err := randomKey()
			if err != nil {
				return nil, err
			}
			if main.Data == nil {
				main.Data = map[string][]byte{}
			}
			main.Data[settings.SecretKeyServerSecretKey] = key
			o.notes = append(o.notes, fmt.Sprintf("%s has no %s; patrol-server generates it at its first start (validated as if present)", o.secretPath, settings.SecretKeyServerSecretKey))
		}
		addSecret(main)
		for _, s := range secrets {
			if s != sec {
				addSecret(s)
			}
		}
		fromFiles = true
	}
	for _, path := range o.files {
		cms, secrets, err := manifestObjects(path)
		if err != nil {
			return nil, err
		}
		for _, cm := range cms {
			addCM(cm)
		}
		for _, s := range secrets {
			addSecret(s)
		}
		fromFiles = true
	}
	if o.loadCluster {
		if err := o.loadFromCluster(ctx, addCM, addSecret); err != nil {
			return nil, err
		}
	} else if o.secretPath == "" && !byName["secret/"+settings.DefaultSecretName] {
		key, err := randomKey()
		if err != nil {
			return nil, err
		}
		addSecret(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: settings.DefaultSecretName}, Data: map[string][]byte{settings.SecretKeyServerSecretKey: key}})
	}
	// Offline files are validated without the part-of label requirement (upstream labels the
	// objects it reads); objects read from the cluster alone are checked as components see them.
	in.IgnorePartOfLabel = fromFiles
	return settings.Parse(in), nil
}

func (o *settingsOptions) loadFromCluster(ctx context.Context, addCM func(*corev1.ConfigMap), addSecret func(*corev1.Secret)) error {
	kc, err := o.kube.get(nil)
	if err != nil {
		return err
	}
	names := settings.DefaultNames()
	for _, name := range []string{names.ConfigMap, names.RBACConfigMap, names.CmdParamsConfigMap, names.TLSCertsConfigMap,
		names.SSHKnownHostsConfigMap, names.GPGKeysConfigMap, names.NotificationsConfigMap} {
		cm, err := kc.kube.CoreV1().ConfigMaps(kc.namespace).Get(ctx, name, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			if name == names.ConfigMap {
				return fmt.Errorf("error getting ConfigMap %s/%s: %w", kc.namespace, name, err)
			}
			continue
		case err != nil:
			return fmt.Errorf("error getting ConfigMap %s/%s: %w", kc.namespace, name, err)
		}
		addCM(cm)
	}
	for _, name := range []string{names.Secret, names.NotificationsSecret} {
		sec, err := kc.kube.CoreV1().Secrets(kc.namespace).Get(ctx, name, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			if name == names.Secret {
				return fmt.Errorf("error getting Secret %s/%s: %w", kc.namespace, name, err)
			}
			continue
		case err != nil:
			return fmt.Errorf("error getting Secret %s/%s: %w", kc.namespace, name, err)
		}
		addSecret(sec)
	}
	refs, err := kc.kube.CoreV1().Secrets(kc.namespace).List(ctx, metav1.ListOptions{LabelSelector: settings.LabelKeyPartOf + "=" + settings.LabelValuePartOf})
	if err != nil {
		return fmt.Errorf("error listing Secrets: %w", err)
	}
	for i := range refs.Items {
		addSecret(&refs.Items[i])
	}
	return nil
}

func newValidateSettingsCommand(f *Factory, o *settingsOptions) *cobra.Command {
	var groups []string
	all := settings.ValidationGroups()
	cmd := &cobra.Command{
		Use:   "validate",
		Short: "Validate settings",
		Long:  "Validates settings specified in 'patrol-cm' ConfigMap and 'patrol-secret' Secret (and the other settings objects when loaded from the cluster or given with --settings-file)",
		Example: `
#Validates all settings in the specified YAML file
patrol admin settings validate --patrol-cm-path ./patrol-cm.yaml

#Validates accounts and RBAC settings in Kubernetes cluster of current kubeconfig context
patrol admin settings validate --group accounts --group rbac --load-cluster-settings`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			s, err := o.load(c.Context())
			if err != nil {
				return err
			}
			for _, n := range o.notes {
				_, _ = fmt.Fprintf(f.errOut(), "note: %s\n", n)
			}
			return printValidation(f.out(), s, groups)
		},
	}
	cmd.Flags().StringArrayVar(&groups, "group", nil, fmt.Sprintf("Optional list of setting groups that have to be validated ( one of: %s)", strings.Join(all, ", ")))
	return cmd
}

// printValidation prints one block per group in upstream's format: "✅ <group>" and the
// summary, or "❌ <group>" and the errors; warnings follow. The exit status is 0 even when a
// group fails, as upstream.
func printValidation(w io.Writer, s *settings.Settings, groups []string) error {
	results, err := settings.ValidateGroups(s, groups...)
	if err != nil {
		return err
	}
	for i, r := range results {
		if r.OK {
			_, _ = fmt.Fprintf(w, "✅ %s\n", r.Group)
			if r.Summary != "" {
				_, _ = fmt.Fprintf(w, "%s\n", r.Summary)
			}
		} else {
			_, _ = fmt.Fprintf(w, "❌ %s\n", r.Group)
			for _, is := range r.Issues {
				if is.IsError() {
					_, _ = fmt.Fprintf(w, "%s\n", is.String())
				}
			}
		}
		for _, is := range r.Issues {
			if !is.IsError() {
				_, _ = fmt.Fprintf(w, "%s\n", is.String())
			}
		}
		if i != len(results)-1 {
			_, _ = fmt.Fprint(w, "\n")
		}
	}
	return nil
}

// readResourceManifest reads the first object of a local manifest.
func readResourceManifest(path string) (*unstructured.Unstructured, error) {
	data, err := readFileLimited(path)
	if err != nil {
		return nil, err
	}
	objs, err := splitObjects(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(objs) == 0 {
		return nil, fmt.Errorf("%s contains no object", path)
	}
	return objs[0], nil
}
