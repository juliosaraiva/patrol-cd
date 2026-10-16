package admin

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
)

func newProjCommand(f *Factory, kube *kubeOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "proj",
		Short: "Manage projects configuration",
		RunE:  func(c *cobra.Command, _ []string) error { return c.Help() },
	}
	cmd.AddCommand(newProjGenerateSpecCommand(f), newUpdateRolePolicyCommand(f, kube), newGenerateAllowListCommand(f, kube))
	return cmd
}

// projOptions are the AppProject flags shared with "patrol proj create" (upstream
// ProjectOpts).
type projOptions struct {
	description                string
	destinations               []string
	destinationServiceAccounts []string
	sources                    []string
	signatureKeys              []string
	sourceNamespaces           []string
	orphanedResources          bool
	orphanedResourcesWarn      bool
	allowedClusterResources    []string
	deniedClusterResources     []string
	allowedNamespacedResources []string
	deniedNamespacedResources  []string
}

func addProjFlags(cmd *cobra.Command, o *projOptions) {
	fl := cmd.Flags()
	fl.StringVar(&o.description, "description", "", "Project description")
	fl.StringArrayVarP(&o.destinations, "dest", "d", []string{}, "Permitted destination server and namespace (e.g. https://192.168.99.100:8443,default)")
	fl.StringArrayVarP(&o.sources, "src", "s", []string{}, "Permitted source repository URL")
	fl.StringSliceVar(&o.signatureKeys, "signature-keys", []string{}, "GnuPG public key IDs for commit signature verification")
	fl.BoolVar(&o.orphanedResources, "orphaned-resources", false, "Enables orphaned resources monitoring")
	fl.BoolVar(&o.orphanedResourcesWarn, "orphaned-resources-warn", false, "Specifies if applications should have a warning condition when orphaned resources detected")
	fl.StringArrayVar(&o.allowedClusterResources, "allow-cluster-resource", []string{}, "List of allowed cluster level resources, optionally with group and name (e.g. ClusterRole, apiextensions.k8s.io/CustomResourceDefinition, /Namespace/team1-*)")
	fl.StringArrayVar(&o.deniedClusterResources, "deny-cluster-resource", []string{}, "List of denied cluster level resources, optionally with group and name (e.g. ClusterRole, apiextensions.k8s.io/CustomResourceDefinition, /Namespace/kube-*)")
	fl.StringArrayVar(&o.allowedNamespacedResources, "allow-namespaced-resource", []string{}, "List of allowed namespaced resources")
	fl.StringArrayVar(&o.deniedNamespacedResources, "deny-namespaced-resource", []string{}, "List of denied namespaced resources")
	fl.StringSliceVar(&o.sourceNamespaces, "source-namespaces", []string{}, "List of source namespaces for applications")
	fl.StringArrayVar(&o.destinationServiceAccounts, "dest-service-accounts", []string{}, "Destination server, namespace and target service account (e.g. https://192.168.99.100:8443,default,default-sa)")
}

func groupKinds(values []string) []metav1.GroupKind {
	var out []metav1.GroupKind
	for _, v := range values {
		switch parts := strings.Split(v, "/"); len(parts) {
		case 2:
			out = append(out, metav1.GroupKind{Group: parts[0], Kind: parts[1]})
		case 1:
			out = append(out, metav1.GroupKind{Kind: parts[0]})
		}
	}
	return out
}

func clusterRestrictions(values []string) []v1alpha1.ClusterResourceRestrictionItem {
	var out []v1alpha1.ClusterResourceRestrictionItem
	for _, v := range values {
		switch parts := strings.Split(v, "/"); len(parts) {
		case 3:
			out = append(out, v1alpha1.ClusterResourceRestrictionItem{Group: parts[0], Kind: parts[1], Name: parts[2]})
		case 2:
			out = append(out, v1alpha1.ClusterResourceRestrictionItem{Group: parts[0], Kind: parts[1]})
		case 1:
			out = append(out, v1alpha1.ClusterResourceRestrictionItem{Kind: parts[0]})
		}
	}
	return out
}

// setProjSpecOptions applies the project flags that were set (upstream SetProjSpecOptions).
func (o *projOptions) setProjSpecOptions(flags *pflag.FlagSet, spec *v1alpha1.AppProjectSpec) error {
	var err error
	flags.Visit(func(fl *pflag.Flag) {
		if err != nil {
			return
		}
		switch fl.Name {
		case "description":
			spec.Description = o.description
		case "dest":
			spec.Destinations = make([]v1alpha1.ApplicationDestination, 0, len(o.destinations))
			for _, d := range o.destinations {
				parts := strings.Split(d, ",")
				if len(parts) != 2 {
					err = fmt.Errorf("Expected destination of the form: server,namespace. Received: %s", d) //nolint:staticcheck // upstream wording
					return
				}
				spec.Destinations = append(spec.Destinations, v1alpha1.ApplicationDestination{Server: parts[0], Namespace: parts[1]})
			}
		case "src":
			spec.SourceRepos = o.sources
		case "signature-keys":
			spec.SignatureKeys = make([]v1alpha1.SignatureKey, 0, len(o.signatureKeys)) //nolint:staticcheck // deprecated field kept for parity
			for _, k := range o.signatureKeys {
				id, kerr := settings.GPGKeyID(k)
				if kerr != nil {
					err = kerr
					return
				}
				spec.SignatureKeys = append(spec.SignatureKeys, v1alpha1.SignatureKey{KeyID: id}) //nolint:staticcheck // deprecated field kept for parity
			}
		case "allow-cluster-resource":
			spec.ClusterResourceWhitelist = clusterRestrictions(o.allowedClusterResources)
		case "deny-cluster-resource":
			spec.ClusterResourceBlacklist = clusterRestrictions(o.deniedClusterResources)
		case "allow-namespaced-resource":
			spec.NamespaceResourceWhitelist = groupKinds(o.allowedNamespacedResources)
		case "deny-namespaced-resource":
			spec.NamespaceResourceBlacklist = groupKinds(o.deniedNamespacedResources)
		case "source-namespaces":
			spec.SourceNamespaces = o.sourceNamespaces
		case "dest-service-accounts":
			spec.DestinationServiceAccounts = make([]v1alpha1.ApplicationDestinationServiceAccount, 0, len(o.destinationServiceAccounts))
			for _, d := range o.destinationServiceAccounts {
				parts := strings.Split(d, ",")
				if len(parts) != 3 {
					err = fmt.Errorf("Expected destination service account of the form: server,namespace, defaultServiceAccount. Received: %s", d) //nolint:staticcheck // upstream wording
					return
				}
				spec.DestinationServiceAccounts = append(spec.DestinationServiceAccounts, v1alpha1.ApplicationDestinationServiceAccount{Server: parts[0], Namespace: parts[1], DefaultServiceAccount: parts[2]})
			}
		}
	})
	if err != nil {
		return err
	}
	if flags.Changed("orphaned-resources") || flags.Changed("orphaned-resources-warn") {
		spec.OrphanedResources = nil
		if o.orphanedResources || flags.Changed("orphaned-resources-warn") {
			spec.OrphanedResources = &v1alpha1.OrphanedResourcesMonitorSettings{}
			if flags.Changed("orphaned-resources-warn") {
				w := o.orphanedResourcesWarn
				spec.OrphanedResources.Warn = &w
			}
		}
	}
	return nil
}

func newProjGenerateSpecCommand(f *Factory) *cobra.Command {
	var (
		o       projOptions
		fileURL string
		output  string
		inline  bool
	)
	cmd := &cobra.Command{
		Use:   "generate-spec PROJECT",
		Short: "Generate declarative config for a project",
		Example: `
  # Generate a YAML configuration for a project named "myproject"
  patrol admin proj generate-spec myproject

  # Generate a JSON configuration for a project named "anotherproject" and specify an output file
  patrol admin proj generate-spec anotherproject --output json --file config.json

  # Generate a YAML configuration for a project named "someproject" and write it back to the input file
  patrol admin proj generate-spec someproject --inline --file someproject.yaml
`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			proj := &v1alpha1.AppProject{TypeMeta: metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: "AppProject"}}
			switch {
			case fileURL != "":
				var data []byte
				var err error
				if fileURL == "-" {
					data, err = readSource("-", f.in())
				} else {
					data, err = readFileOrURL(c.Context(), fileURL)
				}
				if err != nil {
					return fmt.Errorf("error reading proj from uri: %w", err)
				}
				if err := yaml.Unmarshal(data, proj); err != nil {
					return fmt.Errorf("error reading proj from uri: %w", err)
				}
				if len(args) == 1 && fileURL != "-" && args[0] != proj.Name {
					return fmt.Errorf("project name '%s' does not match project spec metadata.name '%s'", args[0], proj.Name)
				}
			case len(args) == 0:
				_ = c.Help()
				return exitWith(1, nil)
			default:
				proj.Name = args[0]
			}
			if err := o.setProjSpecOptions(c.Flags(), &proj.Spec); err != nil {
				return err
			}
			w, closeOut, err := outWriter(inline, fileURL, f.out())
			if err != nil {
				return err
			}
			return errors.Join(printResources(w, output, proj), closeOut())
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", formatYAML, "Output format. One of: json|yaml")
	cmd.Flags().StringVarP(&fileURL, "file", "f", "", "Filename or URL to Kubernetes manifests for the project")
	cmd.Flags().BoolVarP(&inline, "inline", "i", false, "If set then generated resource is written back to the file specified in --file flag")
	_ = cmd.Flags().SetAnnotation("file", cobra.BashCompFilenameExt, []string{"json", "yaml", "yml"})
	addProjFlags(cmd, &o)
	return cmd
}

// projectGlobMatch is upstream's project and role pattern match ("*" or filepath.Match).
func projectGlobMatch(pattern, val string) bool {
	if pattern == "*" {
		return true
	}
	ok, err := filepath.Match(pattern, val)
	return ok && err == nil
}

// policyModification returns the policy text for (project, action), "" meaning remove.
func policyModification(modification, resource, scope, permission string) (func(proj, action string) string, error) {
	switch modification {
	case "set":
		if scope == "" {
			return nil, errors.New("flag --group cannot be empty if permission should be set in role")
		}
		if permission == "" {
			return nil, errors.New("flag --permission cannot be empty if permission should be set in role")
		}
		return func(proj, action string) string {
			return fmt.Sprintf("%s, %s, %s/%s, %s", resource, action, proj, scope, permission)
		}, nil
	case "remove":
		return func(string, string) string { return "" }, nil
	}
	return nil, fmt.Errorf("modification %s is not supported", modification)
}

func trimmedFields(s string) []string {
	parts := strings.Split(s, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

// updateRolePolicies applies a modification to the roles of proj matching rolePattern. It
// reports whether the project changed.
func updateRolePolicies(proj *v1alpha1.AppProject, rolePattern, action string, modify func(proj, action string) string) bool {
	updated := false
	for i, role := range proj.Spec.Roles {
		if !projectGlobMatch(rolePattern, role.Name) {
			continue
		}
		idx := -1
		for j, p := range role.Policies {
			if parts := trimmedFields(p); len(parts) == 6 && parts[3] == action {
				idx = j
				break
			}
		}
		perm := modify(proj.Name, action)
		policy := fmt.Sprintf("p, proj:%s:%s, %s", proj.Name, role.Name, perm)
		switch {
		case idx == -1 && perm != "":
			role.Policies = append(role.Policies, policy)
			updated = true
		case idx > -1 && perm == "":
			role.Policies = append(role.Policies[:idx], role.Policies[idx+1:]...)
			updated = true
		case idx > -1 && perm != "" && role.Policies[idx] != policy:
			role.Policies[idx] = policy
			updated = true
		}
		proj.Spec.Roles[i] = role
	}
	return updated
}

func newUpdateRolePolicyCommand(f *Factory, kube *kubeOptions) *cobra.Command {
	var (
		resource, scope, rolePattern, permission string
		dryRun                                   bool
	)
	cmd := &cobra.Command{
		Use:   "update-role-policy PROJECT_GLOB MODIFICATION ACTION",
		Short: "Implement bulk project role update. Useful to back-fill existing project policies or remove obsolete actions.",
		Example: `  # Add policy that allows executing any action (action/*) to roles which name matches to *deployer* in all projects
  patrol admin proj update-role-policy '*' set 'action/*' --role '*deployer*' --resource applications --scope '*' --permission allow

  # Remove policy that which manages running (action/*) from all roles which name matches *deployer* in all projects
  patrol admin proj update-role-policy '*' remove override --role '*deployer*'
`,
		Args: cobra.ExactArgs(3),
		RunE: func(c *cobra.Command, args []string) error {
			ctx := c.Context()
			modify, err := policyModification(args[1], resource, scope, permission)
			if err != nil {
				return err
			}
			kc, err := kube.get(func(cfg *rest.Config) { cfg.QPS, cfg.Burst = 100, 50 })
			if err != nil {
				return err
			}
			var list v1alpha1.AppProjectList
			if err := kc.ctrl.List(ctx, &list, client.InNamespace(kc.namespace)); err != nil {
				return fmt.Errorf("error listing the projects: %w", err)
			}
			for i := range list.Items {
				proj := &list.Items[i]
				if !projectGlobMatch(args[0], proj.Name) {
					continue
				}
				orig := proj.DeepCopy()
				if !updateRolePolicies(proj, rolePattern, args[2], modify) {
					continue
				}
				if err := saveProject(ctx, f, kc, proj, orig, dryRun); err != nil {
					return fmt.Errorf("error saving the project: %w", err)
				}
			}
			return nil
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&resource, "resource", "", "Resource e.g. 'applications'")
	fl.StringVar(&scope, "scope", "", "Resource scope e.g. '*'")
	fl.StringVar(&rolePattern, "role", "*", "Role name pattern e.g. '*deployer*'")
	fl.StringVar(&permission, "permission", "", "Action permission")
	fl.BoolVar(&dryRun, "dry-run", true, "Dry run")
	return cmd
}

// saveProject prints the change of a project and, unless dry-run, updates it.
func saveProject(ctx context.Context, f *Factory, kc *kubeClients, updated, orig *v1alpha1.AppProject, dryRun bool) error {
	_, _ = fmt.Fprintf(f.out(), "===== %s ======\n", updated.Name)
	target, err := toMap(updated)
	if err != nil {
		return err
	}
	live, err := toMap(orig)
	if err != nil {
		return err
	}
	if err := printObjectDiff(f.out(), updated.Name, &unstructured.Unstructured{Object: specOnly(live)}, &unstructured.Unstructured{Object: specOnly(target)}); err != nil {
		return err
	}
	if dryRun {
		return nil
	}
	if err := kc.ctrl.Update(ctx, updated); err != nil {
		return fmt.Errorf("error while updating project:  %w", err)
	}
	return nil
}

func newGenerateAllowListCommand(f *Factory, kube *kubeOptions) *cobra.Command {
	var out string
	cmd := &cobra.Command{
		Use:   "generate-allow-list CLUSTERROLE_PATH PROJ_NAME",
		Short: "Generates project allow list from the specified clusterRole file",
		Example: `# Generates project allow list from the specified clusterRole file
patrol admin proj generate-allow-list /path/to/clusterrole.yaml my-project`,
		Args: cobra.ExactArgs(2),
		RunE: func(c *cobra.Command, args []string) error {
			kc, err := kube.get(nil)
			if err != nil {
				return err
			}
			resources, err := kc.discovery.ServerPreferredResources()
			if err != nil {
				return fmt.Errorf("error while getting server resources: %w", err)
			}
			proj, err := generateProjectAllowList(resources, args[0], args[1])
			if err != nil {
				return err
			}
			data, err := yaml.Marshal(proj)
			if err != nil {
				return err
			}
			target, err := openOutput(out, f.out())
			if err != nil {
				return err
			}
			_, werr := target.Write(data)
			return errors.Join(werr, target.Close())
		},
	}
	cmd.Flags().StringVarP(&out, "out", "o", "-", "Output to the specified file instead of stdout")
	return cmd
}

// generateProjectAllowList builds an AppProject whose namespaceResourceWhitelist has a
// group/kind for every resource the ClusterRole may create (upstream semantics: rules
// without apiGroups are skipped, only the first apiGroup of a rule is considered, resource
// names are resolved to kinds through discovery).
func generateProjectAllowList(serverResources []*metav1.APIResourceList, clusterRolePath, projName string) (*v1alpha1.AppProject, error) {
	data, err := readFileLimited(clusterRolePath)
	if err != nil {
		return nil, fmt.Errorf("error reading cluster role file: %w", err)
	}
	role := &rbacv1.ClusterRole{}
	if err := yaml.Unmarshal(data, role); err != nil {
		return nil, fmt.Errorf("error unmarshalling cluster role file yaml: %w", err)
	}
	if role.Kind != "" && role.Kind != "ClusterRole" {
		return nil, fmt.Errorf("error converting cluster role yaml into ClusterRole struct: unexpected kind %q", role.Kind)
	}
	list := make([]metav1.GroupKind, 0)
	for _, rule := range role.Rules {
		if len(rule.APIGroups) == 0 {
			continue
		}
		canCreate := false
		for _, verb := range rule.Verbs {
			if strings.EqualFold(verb, "create") {
				canCreate = true
				break
			}
		}
		if !canCreate {
			continue
		}
		group := rule.APIGroups[0]
		for _, res := range rule.Resources {
			for _, rl := range serverResources {
				gv, err := schema.ParseGroupVersion(rl.GroupVersion)
				if err != nil {
					gv = schema.GroupVersion{}
				}
				if gv.Group != group {
					continue
				}
				for _, ar := range rl.APIResources {
					if ar.Name == res {
						list = append(list, metav1.GroupKind{Group: group, Kind: ar.Kind})
					}
				}
			}
		}
	}
	proj := &v1alpha1.AppProject{
		TypeMeta:   metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: "AppProject"},
		ObjectMeta: metav1.ObjectMeta{Name: projName},
	}
	proj.Spec.NamespaceResourceWhitelist = list
	return proj, nil
}
