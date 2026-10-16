package admin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/juliosaraiva/patrol-cd/internal/settings"
)

type exportOptions struct {
	out               string
	appNamespaces     []string
	appSetNamespaces  []string
	namespaceScoped   bool
	includeSecrets    bool
	includeOperations bool
}

func newExportCommand(f *Factory, kube *kubeOptions) *cobra.Command {
	var o exportOptions
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Export all Patrol CD data to stdout (default) or a file",
		Long: `Export the Patrol CD configuration of the control-plane namespace as a YAML stream: the
settings ConfigMaps, the Secrets of the configuration (patrol-secret, credentials of
repositories, credential templates, clusters and SCM providers, Secrets referenced from
settings), AppProjects, SCMProviders, Repositories, RepositoryCredentials, Clusters,
ApplicationSets and Applications (also from the enabled additional namespaces).

Objects are written without uid, resourceVersion, timestamps, managedFields and status, so
they can be imported into a new installation with "patrol admin import".

The export contains credentials unless --include-secrets=false: Secret data is base64
encoded, which is not encryption. Files written with -o are created with mode 0600. A backup
without Secrets restores no credentials, and "patrol admin import --prune" of it keeps every
live Secret.`,
		Example: `# Export to a file
patrol admin export -n patrol -o backup.yaml

# Export without Secrets (credentials must then be restored separately)
patrol admin export --include-secrets=false > backup.yaml`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return runExport(c.Context(), f, kube, o)
		},
	}
	fl := cmd.Flags()
	fl.StringVarP(&o.out, "out", "o", "-", "Output to the specified file instead of stdout")
	fl.StringSliceVar(&o.appNamespaces, "application-namespaces", nil, fmt.Sprintf(
		"Comma-separated list of namespace globs to export applications from, in addition to the control plane namespace. "+
			"If not specified, the value of '%s' in %s is used (if defined in the ConfigMap)", cmdParamsAppNamespacesKey, settings.DefaultCmdParamsConfigMapName))
	fl.StringSliceVar(&o.appSetNamespaces, "applicationset-namespaces", nil, fmt.Sprintf(
		"Comma-separated list of namespace globs to export ApplicationSets from, in addition to the control plane namespace. "+
			"If not specified, the value of '%s' in %s is used (if defined in the ConfigMap)", cmdParamsAppSetNamespacesKey, settings.DefaultCmdParamsConfigMapName))
	fl.BoolVar(&o.namespaceScoped, "namespace-scoped", false, "Namespace-scoped installation: read only the control-plane namespace (no cluster-wide lists, no Applications in other namespaces)")
	fl.BoolVar(&o.includeSecrets, "include-secrets", true, "Include the Secrets of the configuration (credentials). Default true, as upstream, so a backup restores without re-entering credentials")
	fl.BoolVar(&o.includeOperations, "include-operations", false, "Also export Operations (sync history records, with their status). Import never re-creates them")
	return cmd
}

// exportSet is the content of one export, grouped by kind.
type exportSet struct {
	configMaps []*unstructured.Unstructured
	objects    map[patrolKind][]*unstructured.Unstructured
	secrets    []*unstructured.Unstructured
	operations []*unstructured.Unstructured
}

// listPatrol lists the objects of kind k handled by scope. A missing kind (CRD not
// installed) or a forbidden list is a warning, as upstream treats ApplicationSets.
func listPatrol(ctx context.Context, kc *kubeClients, scope nsScope, k patrolKind, warn func(string)) ([]*unstructured.Unstructured, error) {
	list, err := kc.dynamic.Resource(k.gvr(currentGroup)).Namespace(scope.listNamespace(k)).List(ctx, metav1.ListOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) {
			warn(fmt.Sprintf("%s/%s: %v", currentGroup, k.Kind, err))
			return nil, nil
		}
		return nil, fmt.Errorf("error listing %s: %w", k.Resource, err)
	}
	var out []*unstructured.Unstructured
	for i := range list.Items {
		item := &list.Items[i]
		if scope.enabled(k, item.GetNamespace()) {
			out = append(out, item)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].GetNamespace() != out[j].GetNamespace() {
			return out[i].GetNamespace() < out[j].GetNamespace()
		}
		return out[i].GetName() < out[j].GetName()
	})
	return out, nil
}

// collectExport reads everything an export contains.
func collectExport(ctx context.Context, kc *kubeClients, scope nsScope, o exportOptions, warn func(string)) (*exportSet, error) {
	set := &exportSet{objects: map[patrolKind][]*unstructured.Unstructured{}}
	cms := kc.dynamic.Resource(configMapsGVR).Namespace(scope.controlPlane)
	referenced := map[string]bool{}
	for _, name := range requiredSettingsConfigMaps {
		cm, err := cms.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("error getting ConfigMap %s/%s: %w", scope.controlPlane, name, err)
		}
		set.configMaps = append(set.configMaps, cm)
	}
	for _, name := range optionalSettingsConfigMaps {
		cm, err := cms.Get(ctx, name, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			continue
		case err != nil:
			return nil, fmt.Errorf("error getting ConfigMap %s/%s: %w", scope.controlPlane, name, err)
		}
		set.configMaps = append(set.configMaps, cm)
	}
	for _, cm := range set.configMaps {
		data, _, _ := unstructured.NestedStringMap(cm.Object, "data")
		settingsSecretRefs(data, referenced)
	}
	for _, k := range configKinds {
		objs, err := listPatrol(ctx, kc, scope, k, warn)
		if err != nil {
			return nil, err
		}
		set.objects[k] = objs
		if k == kindApplication || k == kindAppProject {
			// Their specs reference no control-plane Secrets.
			continue
		}
		for _, obj := range objs {
			if obj.GetNamespace() == scope.controlPlane {
				secretRefs(obj.Object["spec"], referenced)
			}
		}
	}
	if o.includeSecrets {
		list, err := kc.dynamic.Resource(secretsGVR).Namespace(scope.controlPlane).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("error listing secrets: %w", err)
		}
		for i := range list.Items {
			if isPatrolSecret(&list.Items[i], referenced) {
				set.secrets = append(set.secrets, &list.Items[i])
			}
		}
		sort.SliceStable(set.secrets, func(i, j int) bool { return set.secrets[i].GetName() < set.secrets[j].GetName() })
	}
	if o.includeOperations {
		ops, err := listPatrol(ctx, kc, scope, kindOperation, warn)
		if err != nil {
			return nil, err
		}
		set.operations = ops
	}
	return set, nil
}

// writeExport writes set in import order.
func writeExport(w io.Writer, set *exportSet, controlPlane string) error {
	write := func(objs []*unstructured.Unstructured, keepStatus bool) error {
		for _, obj := range objs {
			if err := writeBackupObject(w, cleanForBackup(obj, controlPlane, keepStatus)); err != nil {
				return err
			}
		}
		return nil
	}
	if err := write(set.configMaps, false); err != nil {
		return err
	}
	for _, k := range configKinds {
		if k == kindApplicationSet {
			// Credentials Secrets follow their owners and precede the objects using them.
			if err := write(set.secrets, false); err != nil {
				return err
			}
		}
		if err := write(set.objects[k], false); err != nil {
			return err
		}
	}
	return write(set.operations, true)
}

func runExport(ctx context.Context, f *Factory, kube *kubeOptions, o exportOptions) error {
	kc, err := kube.get(nil)
	if err != nil {
		return err
	}
	log := f.logger()
	warn := func(msg string) { log.Warn(msg) }
	scope, err := resolveScope(ctx, kc.dynamic, kc.namespace, o.appNamespaces, o.appSetNamespaces, o.namespaceScoped, log)
	if err != nil {
		return err
	}
	set, err := collectExport(ctx, kc, scope, o, warn)
	if err != nil {
		return err
	}
	out, err := openOutput(o.out, f.out())
	if err != nil {
		return err
	}
	werr := writeExport(out, set, kc.namespace)
	if err := errors.Join(werr, out.Close()); err != nil {
		return err
	}
	if o.includeSecrets && len(set.secrets) > 0 {
		_, _ = fmt.Fprintf(f.errOut(), "WARNING: the export contains %d Secret(s) with credentials (base64 is not encryption); store it like a credential, or pass --include-secrets=false\n", len(set.secrets))
	}
	return nil
}
