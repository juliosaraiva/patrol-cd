package admin

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
)

// exitBlocking is the exit code of argocd-import when the export has blocking
// incompatibilities.
const exitBlocking = 2

type argoImportOptions struct {
	dryRun            bool
	output            string
	includeSecrets    bool
	sourceNamespace   string
	keepAutoSync      bool
	overwriteSettings bool
}

func newArgoCDImportCommand(f *Factory, kube *kubeOptions) *cobra.Command {
	o := &argoImportOptions{}
	cmd := &cobra.Command{
		Use:   "argocd-import SOURCE",
		Short: "Convert an Argo CD export into Patrol CD objects",
		Long: `Convert an Argo CD export (the output of 'argocd admin export', or any YAML stream of
Argo CD Applications, AppProjects, ApplicationSets, repository, credential template and
cluster Secrets, and argocd-* settings ConfigMaps and Secrets) into Patrol CD objects.
SOURCE is a file, or '-' for stdin.

The default is a dry run that prints a conversion report and changes nothing. The report
lists every object produced and every finding:

  WARNING   the conversion changed behavior you should review: Argo CD finalizers,
            resource tracking (with the transfer procedure), credentials, automated sync
            with prune/selfHeal (disabled on import unless --keep-auto-sync), notification
            subscriptions, settings that need attention.
  BLOCKING  the object cannot be converted (unknown fields, unsupported kinds, invalid
            Secrets). The command exits with code 2 and never writes anything.

Secret values are never printed. --output writes the converted objects to a file created
with mode 0600 (Secrets included unless --include-secrets=false). --dry-run=false creates
the objects in the Patrol CD namespace; existing objects are left unchanged (settings
ConfigMaps and Secrets are merged with --overwrite-settings).`,
		Example: `# Review the conversion of an Argo CD export
argocd admin export -n argocd > argocd-export.yaml
patrol admin argocd-import argocd-export.yaml

# Write the converted objects to a file for review
patrol admin argocd-import argocd-export.yaml -o patrol-objects.yaml

# Create the objects in namespace patrol
patrol admin argocd-import argocd-export.yaml -n patrol --dry-run=false`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			return runArgoCDImport(c.Context(), f, kube, args[0], o)
		},
	}
	fl := cmd.Flags()
	fl.BoolVar(&o.dryRun, "dry-run", true, "Print the conversion report without creating objects")
	fl.StringVarP(&o.output, "output", "o", "", "Also write the converted objects as YAML to this file (mode 0600)")
	fl.BoolVar(&o.includeSecrets, "include-secrets", true, "Include Secrets (credentials) in the --output file")
	fl.StringVar(&o.sourceNamespace, "source-namespace", argoDefaultNamespace, "Namespace Argo CD was installed in; objects of other namespaces keep their namespace")
	fl.BoolVar(&o.keepAutoSync, "keep-auto-sync", false, "Keep automated sync enabled on imported Applications and ApplicationSet templates (default: disable it)")
	fl.BoolVar(&o.overwriteSettings, "overwrite-settings", false, "Merge converted keys into existing settings ConfigMaps and Secrets (default: leave them unchanged)")
	return cmd
}

func runArgoCDImport(ctx context.Context, f *Factory, kube *kubeOptions, source string, o *argoImportOptions) error {
	data, err := readSource(source, f.in())
	if err != nil {
		return fmt.Errorf("error reading %s: %w", source, err)
	}
	objs, err := splitObjects(data)
	if err != nil {
		return fmt.Errorf("error reading %s: %w", source, err)
	}
	if len(objs) == 0 {
		return fmt.Errorf("%s contains no objects", source)
	}
	conv := convertArgoExport(objs, o.sourceNamespace, o.keepAutoSync)
	out := f.out()
	printConversionReport(out, conv)
	if n := conv.blocking(); n > 0 {
		_, _ = fmt.Fprintf(out, "\nResult: %d blocking incompatibilit%s; nothing was written.\n", n, plural(n, "y", "ies"))
		return exitWith(exitBlocking, fmt.Errorf("%d blocking incompatibilit%s found", n, plural(n, "y", "ies")))
	}
	if o.output != "" {
		n, err := writeConverted(o.output, conv, o.includeSecrets)
		if err != nil {
			return fmt.Errorf("error writing %s: %w", o.output, err)
		}
		_, _ = fmt.Fprintf(out, "\nWrote %d object(s) to %s (mode 0600).\n", n, o.output)
		if secrets := conv.secretCount(); secrets > 0 {
			if o.includeSecrets {
				_, _ = fmt.Fprintf(f.errOut(), "WARNING: %s contains %d Secret(s) with credentials (base64 is not encryption); store it securely and delete it after use.\n", o.output, secrets)
			} else {
				_, _ = fmt.Fprintf(f.errOut(), "note: %d Secret(s) were left out of %s (--include-secrets=false); the objects referencing them need them.\n", secrets, o.output)
			}
		}
	}
	if o.dryRun {
		_, _ = fmt.Fprintln(out, "\nResult: dry run, nothing was changed. Re-run with --dry-run=false to create the objects.")
		return nil
	}
	kc, err := kube.get(nil)
	if err != nil {
		return err
	}
	a := &argoApplier{f: f, out: out, kc: kc, o: o, uids: map[string]metav1.OwnerReference{}}
	return a.apply(ctx, conv)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func (c *conversion) secretCount() int {
	n := 0
	for _, co := range c.objects {
		if co.Obj.GetKind() == "Secret" {
			n++
		}
	}
	return n
}

// describeConverted names a converted object for the report.
func describeConverted(obj *unstructured.Unstructured) string {
	s := obj.GetKind() + " "
	if ns := obj.GetNamespace(); ns != "" {
		s += ns + "/"
	}
	return s + obj.GetName()
}

// printConversionReport prints the objects produced and the findings. It never prints
// Secret values.
func printConversionReport(w io.Writer, c *conversion) {
	_, _ = fmt.Fprintf(w, "Argo CD export: %d object(s) read, %d Patrol CD object(s) produced\n", c.inputCount, len(c.objects))
	if len(c.objects) > 0 {
		_, _ = fmt.Fprintln(w, "\nOBJECTS")
		tw := newTable(w)
		_, _ = fmt.Fprintln(tw, "SOURCE\tPATROL CD OBJECT")
		for _, co := range c.objects {
			target := describeConverted(co.Obj)
			if co.OwnerKind != "" {
				target += fmt.Sprintf(" (credentials of %s %s)", co.OwnerKind, co.OwnerName)
			}
			_, _ = fmt.Fprintf(tw, "%s\t%s\n", co.Source, target)
		}
		_ = tw.Flush()
	}
	for _, sev := range []string{sevBlocking, sevWarning, sevInfo} {
		var lines []finding
		for _, fd := range c.findings {
			if fd.Severity == sev {
				lines = append(lines, fd)
			}
		}
		if len(lines) == 0 {
			continue
		}
		title := map[string]string{sevBlocking: "BLOCKING", sevWarning: "WARNINGS", sevInfo: "NOTES"}[sev]
		_, _ = fmt.Fprintf(w, "\n%s (%d)\n", title, len(lines))
		for _, fd := range lines {
			_, _ = fmt.Fprintf(w, "%s: %s: %s\n", sev, fd.Object, fd.Message)
		}
	}
}

// writeConverted writes the converted objects to path (mode 0600).
func writeConverted(path string, c *conversion, includeSecrets bool) (int, error) {
	t, err := openOutput(path, nil)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, co := range c.objects {
		if co.Obj.GetKind() == "Secret" && !includeSecrets {
			continue
		}
		if err := writeBackupObject(t, co.Obj); err != nil {
			_ = t.Close()
			return n, err
		}
		n++
	}
	return n, t.Close()
}

type argoApplier struct {
	f   *Factory
	out io.Writer
	kc  *kubeClients
	o   *argoImportOptions
	// uids maps "Kind/name" of created or existing Patrol objects to owner references.
	uids map[string]metav1.OwnerReference
}

var (
	configMapGVR = schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
	secretGVR    = schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
)

// applyRank orders creation: settings, Secrets other than credentials, projects,
// repositories and clusters, their credentials, ApplicationSets, Applications.
func applyRank(co convertedObject) int {
	switch co.Obj.GetKind() {
	case "ConfigMap":
		return 0
	case "Secret":
		if co.OwnerKind != "" {
			return 4
		}
		return 1
	case kindAppProject.Kind:
		return 2
	case kindApplicationSet.Kind:
		return 5
	case kindApplication.Kind:
		return 6
	}
	return 3
}

func (a *argoApplier) apply(ctx context.Context, c *conversion) error {
	ns := a.kc.namespace
	_, _ = fmt.Fprintf(a.out, "\nCreating objects in namespace %s\n", ns)
	created, skipped := 0, 0
	for rank := 0; rank <= 6; rank++ {
		for _, co := range c.objects {
			if applyRank(co) != rank {
				continue
			}
			ok, err := a.applyOne(ctx, co, ns)
			if err != nil {
				return fmt.Errorf("error creating %s: %w", describeConverted(co.Obj), err)
			}
			if ok {
				created++
			} else {
				skipped++
			}
		}
	}
	_, _ = fmt.Fprintf(a.out, "Result: %d object(s) created or updated, %d left unchanged. Automated sync stays as reported above; follow the tracking transfer procedure before enabling it.\n", created, skipped)
	return nil
}

func (a *argoApplier) resource(obj *unstructured.Unstructured) (schema.GroupVersionResource, bool) {
	switch obj.GetKind() {
	case "ConfigMap":
		return configMapGVR, true
	case "Secret":
		return secretGVR, true
	}
	k, ok := patrolKindByName(obj.GetKind())
	if !ok {
		return schema.GroupVersionResource{}, false
	}
	return k.gvr(currentGroup), true
}

func isSettingsObject(obj *unstructured.Unstructured) bool {
	switch obj.GetKind() {
	case "ConfigMap":
		return true
	case "Secret":
		return obj.GetName() == settings.DefaultSecretName || obj.GetName() == settings.DefaultNotificationsSecretName
	}
	return false
}

// applyOne creates one object. It returns false when an existing object was left unchanged.
func (a *argoApplier) applyOne(ctx context.Context, co convertedObject, defaultNS string) (bool, error) {
	obj := co.Obj.DeepCopy()
	if obj.GetNamespace() == "" {
		obj.SetNamespace(defaultNS)
	}
	gvr, ok := a.resource(obj)
	if !ok {
		return false, fmt.Errorf("unsupported kind %s", obj.GetKind())
	}
	if co.OwnerKind != "" {
		ref, ok := a.uids[co.OwnerKind+"/"+co.OwnerName]
		if ok {
			obj.SetOwnerReferences([]metav1.OwnerReference{ref})
		}
	}
	client := a.kc.dynamic.Resource(gvr).Namespace(obj.GetNamespace())
	label := describeConverted(obj)
	createdObj, err := client.Create(ctx, obj, metav1.CreateOptions{})
	switch {
	case err == nil:
		a.remember(createdObj)
		_, _ = fmt.Fprintf(a.out, "%s created\n", label)
		return true, nil
	case !apierrors.IsAlreadyExists(err):
		return false, err
	}
	live, err := client.Get(ctx, obj.GetName(), metav1.GetOptions{})
	if err != nil {
		return false, err
	}
	a.remember(live)
	if !isSettingsObject(obj) || !a.o.overwriteSettings {
		hint := ""
		if isSettingsObject(obj) {
			hint = " (merge it with --overwrite-settings)"
		}
		_, _ = fmt.Fprintf(a.out, "%s already exists; left unchanged%s\n", label, hint)
		return false, nil
	}
	field := "data"
	merged := live.DeepCopy()
	m, _, _ := unstructured.NestedMap(merged.Object, field)
	if m == nil {
		m = map[string]any{}
	}
	add, _, _ := unstructured.NestedMap(obj.Object, field)
	for k, v := range add {
		m[k] = v
	}
	if err := unstructured.SetNestedMap(merged.Object, m, field); err != nil {
		return false, err
	}
	labels := merged.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	for k, v := range obj.GetLabels() {
		labels[k] = v
	}
	merged.SetLabels(labels)
	if _, err := client.Update(ctx, merged, metav1.UpdateOptions{}); err != nil {
		return false, err
	}
	_, _ = fmt.Fprintf(a.out, "%s updated (%d key(s) merged)\n", label, len(add))
	return true, nil
}

func (a *argoApplier) remember(obj *unstructured.Unstructured) {
	if objectGroup(obj) != currentGroup {
		return
	}
	a.uids[obj.GetKind()+"/"+obj.GetName()] = metav1.OwnerReference{
		APIVersion: v1alpha1.GroupVersion.String(), Kind: obj.GetKind(), Name: obj.GetName(), UID: obj.GetUID(),
	}
}
