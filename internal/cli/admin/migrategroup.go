package admin

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"github.com/juliosaraiva/patrol-cd/internal/settings"
)

type migrateGroupOptions struct {
	from, to         string
	dryRun           bool
	overwrite        bool
	appNamespaces    []string
	appSetNamespaces []string
	namespaceScoped  bool
}

func newMigrateGroupCommand(f *Factory, kube *kubeOptions) *cobra.Command {
	o := &migrateGroupOptions{}
	cmd := &cobra.Command{
		Use:   "migrate-group --from-group GROUP [--to-group GROUP]",
		Short: "Copy Patrol CD objects from one API group to another (ADR-0002)",
		Long: fmt.Sprintf(`Copy the Patrol CD objects (AppProjects, SCMProviders, Repositories, RepositoryCredentials,
Clusters, ApplicationSets, Applications) of the control-plane namespace (and the enabled
Application namespaces) from API group --from-group to --to-group (default %s), for
example after the development group was renamed (ADR-0002). Both groups' CRDs must be
installed.

Annotation, label and finalizer keys of the old group are rewritten, owner references are
re-pointed to the copies, and credentials Secrets owned by old objects get an additional
owner reference to the copy so that deleting the old objects does not delete them.
Operations (sync history) are not copied. Source objects are never modified or deleted.

The default is a dry run that only prints the plan; pass --dry-run=false to write.`, currentGroup),
		Example: `# Show what would be copied
patrol admin migrate-group --from-group cd.patrol.example --to-group patrolcd.io

# Copy
patrol admin migrate-group --from-group cd.patrol.example --to-group patrolcd.io --dry-run=false`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return runMigrateGroup(c.Context(), f, kube, o)
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&o.from, "from-group", "", "API group to copy objects from (required)")
	fl.StringVar(&o.to, "to-group", currentGroup, "API group to copy objects to")
	fl.BoolVar(&o.dryRun, "dry-run", true, "Print what would be copied without writing")
	fl.BoolVar(&o.overwrite, "overwrite", false, "Update objects that already exist in the target group (default: leave them unchanged)")
	fl.StringSliceVar(&o.appNamespaces, "application-namespaces", nil, fmt.Sprintf("Namespace globs of Applications to copy in addition to the control-plane namespace (default: '%s' of %s)", cmdParamsAppNamespacesKey, settings.DefaultCmdParamsConfigMapName))
	fl.StringSliceVar(&o.appSetNamespaces, "applicationset-namespaces", nil, fmt.Sprintf("Namespace globs of ApplicationSets to copy in addition to the control-plane namespace (default: '%s' of %s)", cmdParamsAppSetNamespacesKey, settings.DefaultCmdParamsConfigMapName))
	fl.BoolVar(&o.namespaceScoped, "namespace-scoped", false, "Namespace-scoped installation: copy only objects of the control-plane namespace")
	_ = cmd.MarkFlagRequired("from-group")
	return cmd
}

type migrator struct {
	f      *Factory
	out    io.Writer
	kc     *kubeClients
	scope  nsScope
	o      *migrateGroupOptions
	suffix string
	// uids maps target-group objects (existing or created) to their UIDs.
	uids map[resourceKey]types.UID
	// copied maps source objects to their copies (for Secret owner references).
	copied map[resourceKey]resourceKey
}

func runMigrateGroup(ctx context.Context, f *Factory, kube *kubeOptions, o *migrateGroupOptions) error {
	if o.from == "" || o.to == "" || o.from == o.to {
		return fmt.Errorf("--from-group and --to-group must name two different API groups")
	}
	if isArgoGroup(o.from) {
		return fmt.Errorf("%s is Argo CD's API group; use 'patrol admin argocd-import'", o.from)
	}
	kc, err := kube.get(nil)
	if err != nil {
		return err
	}
	scope, err := resolveScope(ctx, kc.dynamic, kc.namespace, o.appNamespaces, o.appSetNamespaces, o.namespaceScoped, f.logger())
	if err != nil {
		return err
	}
	m := &migrator{f: f, out: f.out(), kc: kc, scope: scope, o: o, uids: map[resourceKey]types.UID{}, copied: map[resourceKey]resourceKey{}}
	if o.dryRun {
		m.suffix = " (dry run)"
	}
	for _, g := range []string{o.from, o.to} {
		if _, err := kc.dynamic.Resource(kindAppProject.gvr(g)).Namespace(kc.namespace).List(ctx, metav1.ListOptions{Limit: 1}); err != nil {
			if apierrors.IsNotFound(err) {
				return fmt.Errorf("API group %s is not installed (no AppProject CRD)", g)
			}
			return fmt.Errorf("error listing %s AppProjects: %w", g, err)
		}
	}
	counts := map[string]int{}
	for _, k := range configKinds {
		if err := m.migrateKind(ctx, k, counts); err != nil {
			return err
		}
	}
	if err := m.adoptSecrets(ctx); err != nil {
		return err
	}
	m.warnings(ctx)
	_, _ = fmt.Fprintf(m.out, "Migration from %s to %s: %d created, %d updated, %d unchanged, %d skipped%s\n",
		o.from, o.to, counts["created"], counts["updated"], counts["unchanged"], counts["skipped"], m.suffix)
	return nil
}

func (m *migrator) migrateKind(ctx context.Context, k patrolKind, counts map[string]int) error {
	src := m.kc.dynamic.Resource(k.gvr(m.o.from)).Namespace(m.scope.listNamespace(k))
	list, err := src.List(ctx, metav1.ListOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			m.f.logger().Warn(fmt.Sprintf("%s/%s is not installed; skipped", m.o.from, k.Kind))
			return nil
		}
		return fmt.Errorf("error listing %s/%s: %w", m.o.from, k.Kind, err)
	}
	items := list.Items
	slices.SortFunc(items, func(a, b unstructured.Unstructured) int {
		return strings.Compare(a.GetNamespace()+"/"+a.GetName(), b.GetNamespace()+"/"+b.GetName())
	})
	for i := range items {
		obj := &items[i]
		if !m.scope.enabled(k, obj.GetNamespace()) {
			continue
		}
		if obj.GetDeletionTimestamp() != nil {
			_, _ = fmt.Fprintf(m.out, "%s %s/%s is being deleted; skipped\n", k.Kind, obj.GetNamespace(), obj.GetName())
			counts["skipped"]++
			continue
		}
		srcKey := keyOf(obj)
		target := cleanForBackup(obj, "", false)
		target.SetNamespace(obj.GetNamespace())
		rewriteObjectGroup(target, m.o.from, m.o.to)
		m.resolveOwners(ctx, obj, target)
		dstKey := keyOf(target)
		m.copied[srcKey] = dstKey
		dc := m.kc.dynamic.Resource(k.gvr(m.o.to)).Namespace(target.GetNamespace())
		live, err := dc.Get(ctx, target.GetName(), metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			if !m.o.dryRun {
				created, err := dc.Create(ctx, target, metav1.CreateOptions{})
				if err != nil {
					return fmt.Errorf("error creating %s/%s %s: %w", m.o.to, k.Kind, target.GetName(), err)
				}
				m.uids[dstKey] = created.GetUID()
			} else {
				m.uids[dstKey] = ""
			}
			counts["created"]++
			_, _ = fmt.Fprintf(m.out, "%s/%s %s in namespace %s created%s\n", m.o.to, k.Kind, target.GetName(), target.GetNamespace(), m.suffix)
		case err != nil:
			return fmt.Errorf("error getting %s/%s %s: %w", m.o.to, k.Kind, target.GetName(), err)
		default:
			m.uids[dstKey] = live.GetUID()
			if specsEqual(target, live) {
				counts["unchanged"]++
				_, _ = fmt.Fprintf(m.out, "%s/%s %s in namespace %s unchanged%s\n", m.o.to, k.Kind, target.GetName(), target.GetNamespace(), m.suffix)
				continue
			}
			if !m.o.overwrite {
				counts["skipped"]++
				_, _ = fmt.Fprintf(m.out, "%s/%s %s in namespace %s exists and differs; skipped (use --overwrite)%s\n", m.o.to, k.Kind, target.GetName(), target.GetNamespace(), m.suffix)
				continue
			}
			if !m.o.dryRun {
				if _, err := dc.Update(ctx, updateLive(target, live), metav1.UpdateOptions{}); err != nil {
					return fmt.Errorf("error updating %s/%s %s: %w", m.o.to, k.Kind, target.GetName(), err)
				}
			}
			counts["updated"]++
			_, _ = fmt.Fprintf(m.out, "%s/%s %s in namespace %s updated%s\n", m.o.to, k.Kind, target.GetName(), target.GetNamespace(), m.suffix)
		}
	}
	return nil
}

// resolveOwners re-points owner references of the copy: owners of the source group become
// the copied owners (UIDs of the target group); other owners keep their UID (same cluster).
func (m *migrator) resolveOwners(ctx context.Context, src, target *unstructured.Unstructured) {
	refs := target.GetOwnerReferences()
	if len(refs) == 0 {
		return
	}
	srcRefs := src.GetOwnerReferences()
	kept := refs[:0]
	for i, ref := range refs {
		gv, _ := schema.ParseGroupVersion(ref.APIVersion)
		if gv.Group != m.o.to {
			ref.UID = srcRefs[i].UID
			kept = append(kept, ref)
			continue
		}
		key := resourceKey{Group: gv.Group, Kind: ref.Kind, Namespace: target.GetNamespace(), Name: ref.Name}
		uid, ok := m.uids[key]
		if !ok {
			if dc, err := resourceFor(m.kc.dynamic, schema.GroupKind{Group: gv.Group, Kind: ref.Kind}, target.GetNamespace()); err == nil {
				if owner, err := dc.Get(ctx, ref.Name, metav1.GetOptions{}); err == nil {
					uid, ok = owner.GetUID(), true
					m.uids[key] = uid
				}
			}
		}
		switch {
		case ok && uid != "":
			ref.UID = uid
			kept = append(kept, ref)
		case ok && m.o.dryRun:
			kept = append(kept, ref)
		default:
			m.f.logger().Warn(fmt.Sprintf("%s: owner %s %s has no copy in %s; owner reference removed", objectRef(target), ref.Kind, ref.Name, m.o.to))
		}
	}
	if len(kept) == 0 {
		target.SetOwnerReferences(nil)
	} else {
		target.SetOwnerReferences(kept)
	}
}

// adoptSecrets adds, to Secrets owned by copied source objects, an owner reference to the
// copy (and the target group's secret-type label), so that deleting the source objects
// later does not garbage-collect the credentials.
func (m *migrator) adoptSecrets(ctx context.Context) error {
	secrets, err := m.kc.dynamic.Resource(secretsGVR).Namespace(m.scope.controlPlane).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("error listing secrets: %w", err)
	}
	for i := range secrets.Items {
		sec := &secrets.Items[i]
		refs := sec.GetOwnerReferences()
		var add []metav1.OwnerReference
		for _, ref := range refs {
			gv, _ := schema.ParseGroupVersion(ref.APIVersion)
			if gv.Group != m.o.from {
				continue
			}
			dst, ok := m.copied[resourceKey{Group: gv.Group, Kind: ref.Kind, Namespace: sec.GetNamespace(), Name: ref.Name}]
			if !ok {
				continue
			}
			already := slices.ContainsFunc(refs, func(r metav1.OwnerReference) bool {
				g, _ := schema.ParseGroupVersion(r.APIVersion)
				return g.Group == dst.Group && r.Kind == dst.Kind && r.Name == dst.Name
			})
			if already {
				continue
			}
			nr := ref.DeepCopy()
			nr.APIVersion = rewriteAPIVersion(ref.APIVersion, m.o.from, m.o.to)
			nr.UID = m.uids[dst]
			nr.Controller = nil
			add = append(add, *nr)
		}
		if len(add) == 0 {
			continue
		}
		labels := sec.GetLabels()
		newLabels := map[string]string{}
		for k, v := range labels {
			if nk := rewriteGroupKey(k, m.o.from, m.o.to); nk != k {
				if _, exists := labels[nk]; !exists {
					newLabels[nk] = v
				}
			}
		}
		if !m.o.dryRun {
			updated := sec.DeepCopy()
			updated.SetOwnerReferences(append(refs, add...))
			if len(newLabels) > 0 {
				l := updated.GetLabels()
				for k, v := range newLabels {
					l[k] = v
				}
				updated.SetLabels(l)
			}
			if _, err := m.kc.dynamic.Resource(secretsGVR).Namespace(sec.GetNamespace()).Update(ctx, updated, metav1.UpdateOptions{}); err != nil {
				return fmt.Errorf("error updating Secret %s: %w", sec.GetName(), err)
			}
		}
		for _, r := range add {
			_, _ = fmt.Fprintf(m.out, "/Secret %s in namespace %s: owner reference to %s/%s %s added%s\n", sec.GetName(), sec.GetNamespace(), m.o.to, r.Kind, r.Name, m.suffix)
		}
	}
	return nil
}

// warnings explains what the copy does not change.
func (m *migrator) warnings(ctx context.Context) {
	w := m.f.errOut()
	_, _ = fmt.Fprintf(w, "WARNING: resources deployed by the Applications carry %s/tracking-id (and other %s/ keys): stop the %s controller, then sync the copied Applications so they are tracked with %s keys.\n", m.o.from, m.o.from, m.o.from, m.o.to)
	_, _ = fmt.Fprintf(w, "WARNING: Operations (sync history) are not copied; the source objects of %s are left in place: delete them only after the %s installation runs, removing their finalizers first to avoid cascading deletion of deployed resources.\n", m.o.from, m.o.to)
	cm, err := m.kc.kube.CoreV1().ConfigMaps(m.scope.controlPlane).Get(ctx, settings.DefaultConfigMapName, metav1.GetOptions{})
	if err != nil {
		return
	}
	var keys []string
	for k, v := range cm.Data {
		if strings.Contains(k, m.o.from) || strings.Contains(v, m.o.from) {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	for _, k := range keys {
		_, _ = fmt.Fprintf(w, "WARNING: %s key %q mentions %s; update it for %s\n", settings.DefaultConfigMapName, k, m.o.from, m.o.to)
	}
}
