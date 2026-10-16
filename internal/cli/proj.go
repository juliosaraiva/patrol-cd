package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/cli/cmdutil"
	"github.com/juliosaraiva/patrol-cd/internal/cli/output"
	"github.com/juliosaraiva/patrol-cd/internal/git"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// NewProjectCommand returns `patrol proj` (API-066..069, TEN-053..062).
func NewProjectCommand(f *cmdutil.Factory) *cobra.Command {
	c := &cobra.Command{
		Use:     "proj",
		Short:   "Manage projects",
		Aliases: []string{"project"},
		Example: `  # List all available projects
  patrol proj list

  # Create a new project with name PROJECT
  patrol proj create PROJECT

  # Delete the project with name PROJECT
  patrol proj delete PROJECT

  # Edit the information on project with name PROJECT
  patrol proj edit PROJECT`,
		RunE: func(c *cobra.Command, args []string) error { return cmdutil.HelpExit(c, args) },
	}
	c.AddCommand(
		newProjectRoleCommand(f),
		newProjectCreateCommand(f),
		newProjectGetCommand(f),
		newProjectDeleteCommand(f),
		newProjectListCommand(f),
		newProjectSetCommand(f),
		newProjectEditCommand(f),
		newProjectAddSignatureKeyCommand(f),
		newProjectRemoveSignatureKeyCommand(f),
		newProjectAddDestinationCommand(f),
		newProjectRemoveDestinationCommand(f),
		newProjectAddSourceCommand(f),
		newProjectRemoveSourceCommand(f),
		newModifyResourceListCommand(f, "allow-cluster-resource PROJECT GROUP KIND [NAME]", "Adds a cluster-scoped API resource to the allow list and removes it from deny list", `  # Adds a cluster-scoped API resource with specified GROUP and KIND to the allow list and removes it from deny list for project PROJECT
  patrol proj allow-cluster-resource PROJECT GROUP KIND

  # Adds a cluster-scoped API resource with specified GROUP, KIND and NAME pattern to the allow list and removes it from deny list for project PROJECT
  patrol proj allow-cluster-resource PROJECT GROUP KIND NAME`, true, false),
		newModifyResourceListCommand(f, "deny-cluster-resource PROJECT GROUP KIND", "Removes a cluster-scoped API resource from the allow list and adds it to deny list", `  # Removes a cluster-scoped API resource with specified GROUP and KIND from the allow list and adds it to deny list for project PROJECT
  patrol proj deny-cluster-resource PROJECT GROUP KIND`, false, false),
		newModifyResourceListCommand(f, "allow-namespace-resource PROJECT GROUP KIND", "Removes a namespaced API resource from the deny list or add a namespaced API resource to the allow list", `  # Removes a namespaced API resource with specified GROUP and KIND from the deny list or add a namespaced API resource to the allow list for project PROJECT
  patrol proj allow-namespace-resource PROJECT GROUP KIND`, true, true),
		newModifyResourceListCommand(f, "deny-namespace-resource PROJECT GROUP KIND", "Adds a namespaced API resource to the deny list or removes a namespaced API resource from the allow list", `  # Adds a namespaced API resource with specified GROUP and KIND from the deny list or removes a namespaced API resource from the allow list for project PROJECT
  patrol proj deny-namespace-resource PROJECT GROUP KIND`, false, true),
		newProjectWindowsCommand(f),
		newProjectAddOrphanedIgnoreCommand(f),
		newProjectRemoveOrphanedIgnoreCommand(f),
		newProjectAddSourceNamespaceCommand(f),
		newProjectRemoveSourceNamespaceCommand(f),
		newProjectAddDestinationServiceAccountCommand(f),
		newProjectRemoveDestinationServiceAccountCommand(f),
		newProjectSourceIntegrityCommand(f),
	)
	return c
}

// projectOpts are the spec flags of `proj create` and `proj set` (upstream ProjectOpts).
type projectOpts struct {
	description                string
	destinations               []string
	destinationServiceAccounts []string
	sources                    []string
	signatureKeys              []string
	sourceNamespaces           []string
	orphanedResourcesEnabled   bool
	orphanedResourcesWarn      bool
	allowedClusterResources    []string
	deniedClusterResources     []string
	allowedNamespacedResources []string
	deniedNamespacedResources  []string
}

func (o *projectOpts) addFlags(c *cobra.Command) {
	fs := c.Flags()
	fs.StringVar(&o.description, "description", "", "Project description")
	fs.StringArrayVarP(&o.destinations, "dest", "d", []string{}, "Permitted destination server and namespace (e.g. https://192.168.99.100:8443,default)")
	fs.StringArrayVarP(&o.sources, "src", "s", []string{}, "Permitted source repository URL")
	fs.StringSliceVar(&o.signatureKeys, "signature-keys", []string{}, "GnuPG public key IDs for commit signature verification")
	fs.BoolVar(&o.orphanedResourcesEnabled, "orphaned-resources", false, "Enables orphaned resources monitoring")
	fs.BoolVar(&o.orphanedResourcesWarn, "orphaned-resources-warn", false, "Specifies if applications should have a warning condition when orphaned resources detected")
	fs.StringArrayVar(&o.allowedClusterResources, "allow-cluster-resource", []string{}, "List of allowed cluster level resources, optionally with group and name (e.g. ClusterRole, apiextensions.k8s.io/CustomResourceDefinition, /Namespace/team1-*)")
	fs.StringArrayVar(&o.deniedClusterResources, "deny-cluster-resource", []string{}, "List of denied cluster level resources, optionally with group and name (e.g. ClusterRole, apiextensions.k8s.io/CustomResourceDefinition, /Namespace/kube-*)")
	fs.StringArrayVar(&o.allowedNamespacedResources, "allow-namespaced-resource", []string{}, "List of allowed namespaced resources")
	fs.StringArrayVar(&o.deniedNamespacedResources, "deny-namespaced-resource", []string{}, "List of denied namespaced resources")
	fs.StringSliceVar(&o.sourceNamespaces, "source-namespaces", []string{}, "List of source namespaces for applications")
	fs.StringArrayVar(&o.destinationServiceAccounts, "dest-service-accounts", []string{}, "Destination server, namespace and target service account (e.g. https://192.168.99.100:8443,default,default-sa)")
}

func groupKinds(values []string) []metav1.GroupKind {
	var out []metav1.GroupKind
	for _, v := range values {
		parts := strings.Split(v, "/")
		switch len(parts) {
		case 2:
			out = append(out, metav1.GroupKind{Group: parts[0], Kind: parts[1]})
		case 1:
			out = append(out, metav1.GroupKind{Kind: parts[0]})
		}
	}
	return out
}

func clusterResourceItems(values []string) []v1alpha1.ClusterResourceRestrictionItem {
	var out []v1alpha1.ClusterResourceRestrictionItem
	for _, v := range values {
		parts := strings.Split(v, "/")
		switch len(parts) {
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

func (o *projectOpts) destinationsList() ([]v1alpha1.ApplicationDestination, error) {
	out := make([]v1alpha1.ApplicationDestination, 0, len(o.destinations))
	for _, d := range o.destinations {
		parts := strings.Split(d, ",")
		if len(parts) != 2 {
			return nil, cmdutil.Fatalf("Expected destination of the form: server,namespace. Received: %s", d)
		}
		out = append(out, v1alpha1.ApplicationDestination{Server: parts[0], Namespace: parts[1]})
	}
	return out, nil
}

func (o *projectOpts) destinationServiceAccountList() ([]v1alpha1.ApplicationDestinationServiceAccount, error) {
	out := make([]v1alpha1.ApplicationDestinationServiceAccount, 0, len(o.destinationServiceAccounts))
	for _, d := range o.destinationServiceAccounts {
		parts := strings.Split(d, ",")
		if len(parts) != 3 {
			return nil, cmdutil.Fatalf("Expected destination service account of the form: server,namespace, defaultServiceAccount. Received: %s", d)
		}
		out = append(out, v1alpha1.ApplicationDestinationServiceAccount{Server: parts[0], Namespace: parts[1], DefaultServiceAccount: parts[2]})
	}
	return out, nil
}

// keyID validates a GnuPG key ID (16 hex digits) or fingerprint (40 hex digits) and returns
// the upper-case short key ID (upstream sourceintegrity.KeyID).
func keyID(k string) (string, error) {
	isHex := func(s string) bool {
		return strings.IndexFunc(s, func(r rune) bool {
			return (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F')
		}) < 0
	}
	switch {
	case len(k) == 40 && isHex(k):
		return strings.ToUpper(k[24:]), nil
	case len(k) == 16 && isHex(k):
		return strings.ToUpper(k), nil
	}
	return "", fmt.Errorf("'%s' is not a valid GnuPG key ID", k)
}

func (o *projectOpts) signatureKeyList() ([]v1alpha1.SignatureKey, error) {
	out := make([]v1alpha1.SignatureKey, 0, len(o.signatureKeys))
	for _, k := range o.signatureKeys {
		id, err := keyID(k)
		if err != nil {
			return nil, cmdutil.Fatalf("%s", err.Error())
		}
		out = append(out, v1alpha1.SignatureKey{KeyID: id})
	}
	return out, nil
}

// apply sets the spec fields whose flags were given and returns how many were.
func (o *projectOpts) apply(flags *pflag.FlagSet, spec *v1alpha1.AppProjectSpec) (int, error) {
	visited := 0
	var err error
	flags.Visit(func(fl *pflag.Flag) {
		if err != nil {
			return
		}
		visited++
		switch fl.Name {
		case "description":
			spec.Description = o.description
		case "dest":
			spec.Destinations, err = o.destinationsList()
		case "src":
			spec.SourceRepos = o.sources
		case "signature-keys":
			spec.SignatureKeys, err = o.signatureKeyList() //nolint:staticcheck // SA1019: the upstream flags and commands manage the deprecated field
		case "allow-cluster-resource":
			spec.ClusterResourceWhitelist = clusterResourceItems(o.allowedClusterResources)
		case "deny-cluster-resource":
			spec.ClusterResourceBlacklist = clusterResourceItems(o.deniedClusterResources)
		case "allow-namespaced-resource":
			spec.NamespaceResourceWhitelist = groupKinds(o.allowedNamespacedResources)
		case "deny-namespaced-resource":
			spec.NamespaceResourceBlacklist = groupKinds(o.deniedNamespacedResources)
		case "source-namespaces":
			spec.SourceNamespaces = o.sourceNamespaces
		case "dest-service-accounts":
			spec.DestinationServiceAccounts, err = o.destinationServiceAccountList()
		}
	})
	if err != nil {
		return 0, err
	}
	if flags.Changed("orphaned-resources") || flags.Changed("orphaned-resources-warn") {
		warnChanged := flags.Changed("orphaned-resources-warn")
		if o.orphanedResourcesEnabled || warnChanged {
			s := &v1alpha1.OrphanedResourcesMonitorSettings{}
			if warnChanged {
				w := o.orphanedResourcesWarn
				s.Warn = &w
			}
			spec.OrphanedResources = s
		} else {
			spec.OrphanedResources = nil
		}
		visited++
	}
	return visited, nil
}

// maxRemoteManifestBytes bounds manifests read from URLs.
const maxRemoteManifestBytes = 4 << 20

// readManifest reads a project manifest from a file, an http(s) URL or "-" (stdin).
func readManifest(ctx context.Context, f *cmdutil.Factory, src string) ([]byte, error) {
	if u, err := url.ParseRequestURI(src); err == nil && (u.Scheme == "http" || u.Scheme == "https") {
		ctx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
		if err != nil {
			return nil, err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("error reading proj from uri: %w", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("error reading proj from uri: HTTP %d", resp.StatusCode)
		}
		b, err := io.ReadAll(io.LimitReader(resp.Body, maxRemoteManifestBytes+1))
		if err != nil {
			return nil, fmt.Errorf("error reading proj from uri: %w", err)
		}
		if len(b) > maxRemoteManifestBytes {
			return nil, fmt.Errorf("error reading proj from uri: manifest exceeds %d bytes", maxRemoteManifestBytes)
		}
		return b, nil
	}
	b, err := readInputFile(f, src)
	if err != nil {
		if src == "-" {
			return nil, fmt.Errorf("unable to read manifest from stdin: %w", err)
		}
		return nil, fmt.Errorf("error reading proj from uri: %w", err)
	}
	return b, nil
}

func newProjectCreateCommand(f *cmdutil.Factory) *cobra.Command {
	var (
		o       projectOpts
		fileURL string
		upsert  bool
	)
	c := &cobra.Command{
		Use:   "create PROJECT",
		Short: "Create a project",
		Example: `  # Create a new project with name PROJECT
  patrol proj create PROJECT

  # Create a new project with name PROJECT from a file or URL to a Kubernetes manifest
  patrol proj create PROJECT -f FILE|URL`,
		RunE: func(c *cobra.Command, args []string) error {
			proj := &v1alpha1.AppProject{TypeMeta: metav1.TypeMeta{Kind: "AppProject", APIVersion: v1alpha1.GroupVersion.String()}}
			switch {
			case fileURL != "":
				data, err := readManifest(c.Context(), f, fileURL)
				if err != nil {
					return err
				}
				if err := yaml.Unmarshal(data, proj); err != nil {
					if fileURL == "-" {
						return fmt.Errorf("unable to read manifest from stdin: %w", err)
					}
					return fmt.Errorf("error reading proj from uri: %w", err)
				}
				if fileURL != "-" && len(args) == 1 && args[0] != proj.Name {
					return fmt.Errorf("project name '%s' does not match project spec metadata.name '%s'", args[0], proj.Name)
				}
			default:
				if len(args) == 0 {
					return cmdutil.HelpExit(c, args)
				}
				proj.Name = args[0]
			}
			if _, err := o.apply(c.Flags(), &proj.Spec); err != nil {
				return err
			}
			client, err := f.NewClient(c.Context())
			if err != nil {
				return err
			}
			_, err = client.CreateProject(c.Context(), proj, upsert)
			return err
		},
	}
	c.Flags().BoolVar(&upsert, "upsert", false, "Allows to override a project with the same name even if supplied project spec is different from existing spec")
	c.Flags().StringVarP(&fileURL, "file", "f", "", "Filename or URL to Kubernetes manifests for the project")
	_ = c.Flags().SetAnnotation("file", cobra.BashCompFilenameExt, []string{"json", "yaml", "yml"})
	o.addFlags(c)
	return c
}

// updateProject reads PROJECT, applies mutate and writes it back. mutate returns false
// when nothing must be written.
func updateProject(c *cobra.Command, f *cmdutil.Factory, name string, mutate func(p *v1alpha1.AppProject) (bool, error)) error {
	client, err := f.NewClient(c.Context())
	if err != nil {
		return err
	}
	proj, err := client.GetProject(c.Context(), name)
	if err != nil {
		return err
	}
	write, err := mutate(proj)
	if err != nil || !write {
		return err
	}
	_, err = client.UpdateProject(c.Context(), proj)
	return err
}

func newProjectSetCommand(f *cmdutil.Factory) *cobra.Command {
	var o projectOpts
	c := &cobra.Command{
		Use:   "set PROJECT",
		Short: "Set project parameters",
		Example: `  # Set project parameters with some allowed cluster resources [RES1,RES2,...] for project with name PROJECT
  patrol proj set PROJECT --allow-cluster-resource [RES1,RES2,...]

  # Set project parameters with some denied namespaced resources [RES1,RES2,...] for project with name PROJECT
  patrol proj set PROJECT ---deny-namespaced-resource [RES1,RES2,...]`,
		Args:              cmdutil.MinimumArgs(1),
		ValidArgsFunction: CompleteProjects(f, 1),
		RunE: func(c *cobra.Command, args []string) error {
			return updateProject(c, f, args[0], func(p *v1alpha1.AppProject) (bool, error) {
				visited, err := o.apply(c.Flags(), &p.Spec)
				if err != nil {
					return false, err
				}
				if visited == 0 {
					f.Logger().Error("Please set at least one option to update")
					return false, cmdutil.HelpExit(c, args)
				}
				return true, nil
			})
		},
	}
	o.addFlags(c)
	return c
}

const deprecatedSignatureKeys = "Managing project signature keys through CLI is deprecated, migrate to Source Integrity defined in AppProject manifest"

func newProjectAddSignatureKeyCommand(f *cmdutil.Factory) *cobra.Command {
	return &cobra.Command{
		Use:   "add-signature-key PROJECT KEY-ID",
		Short: "Add GnuPG signature key to project (DEPRECATED)",
		Example: `  # Add GnuPG signature key KEY-ID to project PROJECT
  patrol proj add-signature-key PROJECT KEY-ID`,
		Deprecated: deprecatedSignatureKeys,
		Args:       cmdutil.ExactArgs(2),
		RunE: func(c *cobra.Command, args []string) error {
			key, err := keyID(args[1])
			if err != nil {
				return cmdutil.Fatalf("%s", err.Error())
			}
			return updateProject(c, f, args[0], func(p *v1alpha1.AppProject) (bool, error) {
				for _, k := range p.Spec.SignatureKeys { //nolint:staticcheck // SA1019: the upstream flags and commands manage the deprecated field
					if strings.EqualFold(k.KeyID, key) {
						return false, cmdutil.Fatalf("Specified signature key is already defined in project")
					}
				}
				p.Spec.SignatureKeys = append(p.Spec.SignatureKeys, v1alpha1.SignatureKey{KeyID: key}) //nolint:staticcheck // SA1019: the upstream flags and commands manage the deprecated field
				return true, nil
			})
		},
	}
}

func newProjectRemoveSignatureKeyCommand(f *cmdutil.Factory) *cobra.Command {
	return &cobra.Command{
		Use:   "remove-signature-key PROJECT KEY-ID",
		Short: "Remove GnuPG signature key from project (DEPRECATED)",
		Example: `  # Remove GnuPG signature key KEY-ID from project PROJECT
  patrol proj remove-signature-key PROJECT KEY-ID`,
		Deprecated: deprecatedSignatureKeys,
		Args:       cmdutil.ExactArgs(2),
		RunE: func(c *cobra.Command, args []string) error {
			key := args[1]
			if k, err := keyID(key); err == nil {
				key = k
			}
			return updateProject(c, f, args[0], func(p *v1alpha1.AppProject) (bool, error) {
				i := slices.IndexFunc(p.Spec.SignatureKeys, func(k v1alpha1.SignatureKey) bool { return strings.EqualFold(k.KeyID, key) }) //nolint:staticcheck // SA1019: the upstream flags and commands manage the deprecated field
				if i < 0 {
					return false, cmdutil.Fatalf("Specified signature key is not configured for project")
				}
				p.Spec.SignatureKeys = slices.Delete(p.Spec.SignatureKeys, i, i+1) //nolint:staticcheck // SA1019: the upstream flags and commands manage the deprecated field
				return true, nil
			})
		},
	}
}

func newProjectAddDestinationCommand(f *cmdutil.Factory) *cobra.Command {
	var useName bool
	c := &cobra.Command{
		Use:   "add-destination PROJECT SERVER/NAME NAMESPACE",
		Short: "Add project destination",
		Example: `  # Add project destination using a server URL (SERVER) in the specified namespace (NAMESPACE) on the project with name PROJECT
  patrol proj add-destination PROJECT SERVER NAMESPACE

  # Add project destination using a server name (NAME) in the specified namespace (NAMESPACE) on the project with name PROJECT
  patrol proj add-destination PROJECT NAME NAMESPACE --name`,
		Args:              cmdutil.ExactArgs(3),
		ValidArgsFunction: CompleteProjects(f, 1),
		RunE: func(c *cobra.Command, args []string) error {
			ns := args[2]
			dest := v1alpha1.ApplicationDestination{Server: args[1], Namespace: ns}
			if useName {
				dest = v1alpha1.ApplicationDestination{Name: args[1], Namespace: ns}
			}
			return updateProject(c, f, args[0], func(p *v1alpha1.AppProject) (bool, error) {
				for _, d := range p.Spec.Destinations {
					serverExists := dest.Server != "" && d.Server == dest.Server
					nameExists := dest.Name != "" && d.Name == dest.Name
					if d.Namespace == ns && (serverExists || nameExists) {
						return false, cmdutil.Fatalf("Specified destination is already defined in project")
					}
				}
				p.Spec.Destinations = append(p.Spec.Destinations, dest)
				return true, nil
			})
		},
	}
	c.Flags().BoolVar(&useName, "name", false, "Use name as destination instead server")
	return c
}

func newProjectRemoveDestinationCommand(f *cmdutil.Factory) *cobra.Command {
	return &cobra.Command{
		Use:   "remove-destination PROJECT SERVER NAMESPACE",
		Short: "Remove project destination",
		Example: `  # Remove the destination (SERVER) from the specified namespace (NAMESPACE) on the project with name PROJECT
  patrol proj remove-destination PROJECT SERVER NAMESPACE`,
		Args:              cmdutil.ExactArgs(3),
		ValidArgsFunction: CompleteProjects(f, 1),
		RunE: func(c *cobra.Command, args []string) error {
			server, ns := args[1], args[2]
			return updateProject(c, f, args[0], func(p *v1alpha1.AppProject) (bool, error) {
				i := slices.IndexFunc(p.Spec.Destinations, func(d v1alpha1.ApplicationDestination) bool { return d.Namespace == ns && d.Server == server })
				if i < 0 {
					return false, cmdutil.Fatalf("Specified destination does not exist in project")
				}
				p.Spec.Destinations = slices.Delete(p.Spec.Destinations, i, i+1)
				return true, nil
			})
		},
	}
}

func newProjectAddOrphanedIgnoreCommand(f *cmdutil.Factory) *cobra.Command {
	var name string
	c := &cobra.Command{
		Use:   "add-orphaned-ignore PROJECT GROUP KIND",
		Short: "Add a resource to orphaned ignore list",
		Example: `  # Add a resource of the specified GROUP and KIND to orphaned ignore list on the project with name PROJECT
  patrol proj add-orphaned-ignore PROJECT GROUP KIND

  # Add resources of the specified GROUP and KIND using a NAME pattern to orphaned ignore list on the project with name PROJECT
  patrol proj add-orphaned-ignore PROJECT GROUP KIND --name NAME`,
		Args:              cmdutil.ExactArgs(3),
		ValidArgsFunction: CompleteProjects(f, 1),
		RunE: func(c *cobra.Command, args []string) error {
			key := v1alpha1.OrphanedResourceKey{Group: args[1], Kind: args[2], Name: name}
			return updateProject(c, f, args[0], func(p *v1alpha1.AppProject) (bool, error) {
				if p.Spec.OrphanedResources == nil {
					p.Spec.OrphanedResources = &v1alpha1.OrphanedResourcesMonitorSettings{Ignore: []v1alpha1.OrphanedResourceKey{key}}
					return true, nil
				}
				if slices.Contains(p.Spec.OrphanedResources.Ignore, key) {
					return false, cmdutil.Fatalf("Specified resource is already defined in the orphaned ignore list of project")
				}
				p.Spec.OrphanedResources.Ignore = append(p.Spec.OrphanedResources.Ignore, key)
				return true, nil
			})
		},
	}
	c.Flags().StringVar(&name, "name", "", "Resource name pattern")
	return c
}

func newProjectRemoveOrphanedIgnoreCommand(f *cmdutil.Factory) *cobra.Command {
	var name string
	c := &cobra.Command{
		Use:   "remove-orphaned-ignore PROJECT GROUP KIND",
		Short: "Remove a resource from orphaned ignore list",
		Example: `  # Remove a resource of the specified GROUP and KIND from orphaned ignore list on the project with name PROJECT
  patrol proj remove-orphaned-ignore PROJECT GROUP KIND

  # Remove resources of the specified GROUP and KIND using a NAME pattern from orphaned ignore list on the project with name PROJECT
  patrol proj remove-orphaned-ignore PROJECT GROUP KIND --name NAME`,
		Args:              cmdutil.ExactArgs(3),
		ValidArgsFunction: CompleteProjects(f, 1),
		RunE: func(c *cobra.Command, args []string) error {
			key := v1alpha1.OrphanedResourceKey{Group: args[1], Kind: args[2], Name: name}
			return updateProject(c, f, args[0], func(p *v1alpha1.AppProject) (bool, error) {
				if p.Spec.OrphanedResources == nil {
					return false, cmdutil.Fatalf("Specified resource does not exist in the orphaned ignore list of project")
				}
				i := slices.Index(p.Spec.OrphanedResources.Ignore, key)
				if i < 0 {
					return false, cmdutil.Fatalf("Specified resource does not exist in the orphaned ignore of project")
				}
				p.Spec.OrphanedResources.Ignore = slices.Delete(p.Spec.OrphanedResources.Ignore, i, i+1)
				return true, nil
			})
		},
	}
	c.Flags().StringVar(&name, "name", "", "Resource name pattern")
	return c
}

func newProjectAddSourceCommand(f *cmdutil.Factory) *cobra.Command {
	return &cobra.Command{
		Use:   "add-source PROJECT URL",
		Short: "Add project source repository",
		Example: `  # Add a source repository (URL) to the project with name PROJECT
  patrol proj add-source PROJECT URL`,
		Args:              cmdutil.ExactArgs(2),
		ValidArgsFunction: CompleteProjects(f, 1),
		RunE: func(c *cobra.Command, args []string) error {
			u := args[1]
			return updateProject(c, f, args[0], func(p *v1alpha1.AppProject) (bool, error) {
				for _, item := range p.Spec.SourceRepos {
					if item == "*" {
						output.Println(f.Out(), "Source repository '*' already allowed in project")
						return false, nil
					}
					if git.SameURL(item, u) {
						output.Printf(f.Out(), "Source repository '%s' already allowed in project\n", item)
						return false, nil
					}
				}
				p.Spec.SourceRepos = append(p.Spec.SourceRepos, u)
				return true, nil
			})
		},
	}
}

func newProjectRemoveSourceCommand(f *cmdutil.Factory) *cobra.Command {
	return &cobra.Command{
		Use:   "remove-source PROJECT URL",
		Short: "Remove project source repository",
		Example: `  # Remove URL source repository to project PROJECT
  patrol proj remove-source PROJECT URL`,
		Args:              cmdutil.ExactArgs(2),
		ValidArgsFunction: CompleteProjects(f, 1),
		RunE: func(c *cobra.Command, args []string) error {
			u := args[1]
			return updateProject(c, f, args[0], func(p *v1alpha1.AppProject) (bool, error) {
				i := slices.Index(p.Spec.SourceRepos, u)
				if i < 0 {
					output.Printf(f.Out(), "Source repository '%s' does not exist in project\n", u)
					return false, nil
				}
				p.Spec.SourceRepos = slices.Delete(p.Spec.SourceRepos, i, i+1)
				return true, nil
			})
		},
	}
}

func newProjectAddSourceNamespaceCommand(f *cmdutil.Factory) *cobra.Command {
	return &cobra.Command{
		Use:   "add-source-namespace PROJECT NAMESPACE",
		Short: "Add source namespace to the AppProject",
		Example: `  # Add Kubernetes namespace as source namespace to the AppProject where application resources are allowed to be created in.
  patrol proj add-source-namespace PROJECT NAMESPACE`,
		Args:              cmdutil.ExactArgs(2),
		ValidArgsFunction: CompleteProjects(f, 1),
		RunE: func(c *cobra.Command, args []string) error {
			ns := args[1]
			return updateProject(c, f, args[0], func(p *v1alpha1.AppProject) (bool, error) {
				for _, item := range p.Spec.SourceNamespaces {
					if item == "*" || item == ns {
						output.Printf(f.Out(), "Source namespace '%s' already allowed in project\n", item)
						return false, nil
					}
				}
				p.Spec.SourceNamespaces = append(p.Spec.SourceNamespaces, ns)
				return true, nil
			})
		},
	}
}

func newProjectRemoveSourceNamespaceCommand(f *cmdutil.Factory) *cobra.Command {
	return &cobra.Command{
		Use:   "remove-source-namespace PROJECT NAMESPACE",
		Short: "Removes the source namespace from the AppProject",
		Example: `  # Remove source NAMESPACE in PROJECT
  patrol proj remove-source-namespace PROJECT NAMESPACE`,
		Args:              cmdutil.ExactArgs(2),
		ValidArgsFunction: CompleteProjects(f, 1),
		RunE: func(c *cobra.Command, args []string) error {
			ns := args[1]
			return updateProject(c, f, args[0], func(p *v1alpha1.AppProject) (bool, error) {
				i := slices.IndexFunc(p.Spec.SourceNamespaces, func(s string) bool { return s == ns && s != "*" })
				if i < 0 {
					output.Printf(f.Out(), "Source namespace '%s' does not exist in project or cannot be removed\n", ns)
					return false, nil
				}
				p.Spec.SourceNamespaces = slices.Delete(p.Spec.SourceNamespaces, i, i+1)
				return true, nil
			})
		},
	}
}

func modifyNamespacedList(list *[]metav1.GroupKind, add bool, action, group, kind string) (bool, string) {
	i := slices.IndexFunc(*list, func(gk metav1.GroupKind) bool { return gk.Group == group && gk.Kind == kind })
	if add {
		if i >= 0 {
			return false, fmt.Sprintf("Group '%s' and kind '%s' already present in %s namespaced resources", group, kind, action)
		}
		*list = append(*list, metav1.GroupKind{Group: group, Kind: kind})
		return true, fmt.Sprintf("Group '%s' and kind '%s' is added to %s namespaced resources", group, kind, action)
	}
	if i < 0 {
		return false, fmt.Sprintf("Group '%s' and kind '%s' not in %s namespaced resources", group, kind, action)
	}
	*list = slices.Delete(*list, i, i+1)
	return true, fmt.Sprintf("Group '%s' and kind '%s' is removed from %s namespaced resources", group, kind, action)
}

func modifyClusterList(list *[]v1alpha1.ClusterResourceRestrictionItem, add bool, action, group, kind, name string) (bool, string) {
	item := v1alpha1.ClusterResourceRestrictionItem{Group: group, Kind: kind, Name: name}
	i := slices.Index(*list, item)
	if add {
		if i >= 0 {
			return false, fmt.Sprintf("Group '%s', kind '%s', and name '%s' is already present in %s cluster resources", group, kind, name, action)
		}
		*list = append(*list, item)
		return true, fmt.Sprintf("Group '%s', kind '%s', and name '%s' is added to %s cluster resources", group, kind, name, action)
	}
	if i < 0 {
		return false, fmt.Sprintf("Group '%s', kind '%s', and name '%s' not in %s cluster resources", group, kind, name, action)
	}
	*list = slices.Delete(*list, i, i+1)
	return true, fmt.Sprintf("Group '%s', kind '%s', and name '%s' is removed from %s cluster resources", group, kind, name, action)
}

// newModifyResourceListCommand builds the allow/deny cluster/namespace resource commands
// (upstream modifyResourceListCmd). --list allow|white edits the allow list, anything
// else the deny list. Messages that report no change are not printed (upstream).
func newModifyResourceListCommand(f *cmdutil.Factory, use, short, example string, allow, namespaced bool) *cobra.Command {
	listType := "allow"
	if namespaced {
		listType = "deny"
	}
	c := &cobra.Command{
		Use:               use,
		Short:             short,
		Example:           example,
		ValidArgsFunction: CompleteProjects(f, 1),
		RunE: func(c *cobra.Command, args []string) error {
			if namespaced && len(args) != 3 {
				return cmdutil.HelpExit(c, args)
			}
			if !namespaced && (len(args) < 3 || len(args) > 4) {
				return cmdutil.HelpExit(c, args)
			}
			group, kind, name := args[1], args[2], ""
			if !namespaced && len(args) > 3 {
				name = args[3]
			}
			return updateProject(c, f, args[0], func(p *v1alpha1.AppProject) (bool, error) {
				action, add := "denied", !allow
				if listType == "allow" || listType == "white" {
					action, add = "allowed", allow
				}
				var ok bool
				var msg string
				switch {
				case namespaced && action == "allowed":
					ok, msg = modifyNamespacedList(&p.Spec.NamespaceResourceWhitelist, add, action, group, kind)
				case namespaced:
					ok, msg = modifyNamespacedList(&p.Spec.NamespaceResourceBlacklist, add, action, group, kind)
				case action == "allowed":
					ok, msg = modifyClusterList(&p.Spec.ClusterResourceWhitelist, add, action, group, kind, name)
				default:
					ok, msg = modifyClusterList(&p.Spec.ClusterResourceBlacklist, add, action, group, kind, name)
				}
				if ok {
					output.Println(f.ErrOut(), msg)
				}
				return ok, nil
			})
		},
	}
	c.Flags().StringVarP(&listType, "list", "l", listType, "Use deny list or allow list. This can only be 'allow' or 'deny'")
	return c
}

func newProjectDeleteCommand(f *cmdutil.Factory) *cobra.Command {
	return &cobra.Command{
		Use:   "delete PROJECT",
		Short: "Delete project",
		Example: `  # Delete the project with name PROJECT
  patrol proj delete PROJECT`,
		Args:              cmdutil.MinimumArgs(1),
		ValidArgsFunction: CompleteProjects(f, 0),
		RunE: func(c *cobra.Command, args []string) error {
			client, err := f.NewClient(c.Context())
			if err != nil {
				return err
			}
			p := f.OptionalPrompt()
			for _, name := range args {
				ok, err := p.Confirm(fmt.Sprintf("Are you sure you want to delete %s? [y/n]", name))
				if err != nil {
					return err
				}
				if !ok {
					output.Printf(f.Out(), "The command to delete %s was cancelled.\n", name)
					continue
				}
				if err := client.DeleteProject(c.Context(), name); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

func newProjectListCommand(f *cmdutil.Factory) *cobra.Command {
	var format string
	c := &cobra.Command{
		Use:   "list",
		Short: "List projects",
		Example: `  # List all available projects
  patrol proj list

  # List all available projects in yaml format
  patrol proj list -o yaml`,
		RunE: func(c *cobra.Command, _ []string) error {
			if err := output.CheckFormat(format, output.JSON, output.YAML, output.Wide, output.Name, ""); err != nil {
				return output.UnknownFormat(format)
			}
			client, err := f.NewClient(c.Context())
			if err != nil {
				return err
			}
			l, err := client.ListProjects(c.Context(), nil)
			if err != nil {
				return err
			}
			switch format {
			case output.JSON, output.YAML:
				return output.PrintResourceList(f.Out(), l.Items, format, false)
			case output.Name:
				for _, p := range l.Items {
					output.Println(f.Out(), p.Name)
				}
				return nil
			}
			t := output.NewTable(f.Out())
			t.Row("NAME", "DESCRIPTION", "DESTINATIONS", "SOURCES", "CLUSTER-RESOURCE-WHITELIST", "NAMESPACE-RESOURCE-BLACKLIST", "SIGNATURE-KEYS", "ORPHANED-RESOURCES", "DESTINATION-SERVICE-ACCOUNTS")
			for i := range l.Items {
				projectRow(t, &l.Items[i])
			}
			return t.Flush()
		},
	}
	c.Flags().StringVarP(&format, "output", "o", output.Wide, "Output format. One of: json|yaml|wide|name")
	return c
}

func formatOrphanedResources(p *v1alpha1.AppProject) string {
	o := p.Spec.OrphanedResources
	if o == nil {
		return "disabled"
	}
	details := fmt.Sprintf("warn=%v", o.Warn != nil && *o.Warn)
	if len(o.Ignore) > 0 {
		details = fmt.Sprintf("%s, ignored %d", details, len(o.Ignore))
	}
	return fmt.Sprintf("enabled (%s)", details)
}

func countOr[T any](items []T, one func(T) string, many string) string {
	switch len(items) {
	case 0:
		return "<none>"
	case 1:
		return one(items[0])
	}
	return fmt.Sprintf("%d %s", len(items), many)
}

func projectRow(t *output.Tab, p *v1alpha1.AppProject) {
	dests := countOr(p.Spec.Destinations, func(d v1alpha1.ApplicationDestination) string { return d.Server + "," + d.Namespace }, "destinations")
	dsa := countOr(p.Spec.DestinationServiceAccounts, func(d v1alpha1.ApplicationDestinationServiceAccount) string {
		return d.Server + "," + d.Namespace + "," + d.DefaultServiceAccount
	}, "destinationServiceAccounts")
	srcs := countOr(p.Spec.SourceRepos, func(s string) string { return s }, "repos")
	cwl := countOr(p.Spec.ClusterResourceWhitelist, func(i v1alpha1.ClusterResourceRestrictionItem) string { return i.Group + "/" + i.Kind }, "resources")
	nbl := "<none>"
	if n := len(p.Spec.NamespaceResourceBlacklist); n > 0 {
		nbl = fmt.Sprintf("%d resources", n)
	}
	keys := "<none>"
	if n := len(p.Spec.SignatureKeys); n > 0 { //nolint:staticcheck // SA1019: the upstream flags and commands manage the deprecated field
		keys = fmt.Sprintf("%d key(s)", n)
	}
	t.Row(p.Name, p.Spec.Description, dests, srcs, cwl, nbl, keys, formatOrphanedResources(p), dsa)
}

func newProjectGetCommand(f *cmdutil.Factory) *cobra.Command {
	var format string
	c := &cobra.Command{
		Use:   "get PROJECT",
		Short: "Get project details",
		Example: `  # Get details from project PROJECT
  patrol proj get PROJECT

  # Get details from project PROJECT in yaml format
  patrol proj get PROJECT -o yaml`,
		Args:              cmdutil.ExactArgs(1),
		ValidArgsFunction: CompleteProjects(f, 1),
		RunE: func(c *cobra.Command, args []string) error {
			if err := output.CheckFormat(format, output.JSON, output.YAML, output.Wide, ""); err != nil {
				return output.UnknownFormat(format)
			}
			client, err := f.NewClient(c.Context())
			if err != nil {
				return err
			}
			proj, err := client.GetProject(c.Context(), args[0])
			if err != nil {
				return err
			}
			if format == output.JSON || format == output.YAML {
				return output.PrintResource(f.Out(), proj, format)
			}
			detailed, err := client.GetDetailedProject(c.Context(), args[0])
			if err != nil {
				return err
			}
			printProject(f, proj, detailed)
			return nil
		},
	}
	c.Flags().StringVarP(&format, "output", "o", output.Wide, "Output format. One of: json|yaml|wide")
	return c
}

func printProject(f *cmdutil.Factory, p *v1alpha1.AppProject, d *apiclient.ProjectDetailed) {
	const layout = "%-29s%s\n"
	w := f.Out()
	list := func(label string, values []string) {
		if len(values) == 0 {
			output.Printf(w, layout, label, "<none>")
			return
		}
		output.Printf(w, layout, label, values[0])
		for _, v := range values[1:] {
			output.Printf(w, layout, "", v)
		}
	}
	output.Printf(w, layout, "Name:", p.Name)
	output.Printf(w, layout, "Description:", p.Spec.Description)
	var dests []string
	for _, x := range p.Spec.Destinations {
		dests = append(dests, x.Server+","+x.Namespace)
	}
	list("Destinations:", dests)
	list("Repositories:", p.Spec.SourceRepos)
	list("Source Namespaces:", p.Spec.SourceNamespaces)
	var repos []string
	for _, r := range d.Repositories {
		repos = append(repos, r.Spec.URL)
	}
	list("Scoped Repositories:", repos)
	var cwl []string
	for _, x := range p.Spec.ClusterResourceWhitelist {
		cwl = append(cwl, x.Group+"/"+x.Kind)
	}
	list("Allowed Cluster Resources:", cwl)
	var clusters []string
	for _, cl := range d.Clusters {
		clusters = append(clusters, cl.Spec.Server)
	}
	list("Scoped Clusters:", clusters)
	var nbl []string
	for _, x := range p.Spec.NamespaceResourceBlacklist {
		nbl = append(nbl, x.Group+"/"+x.Kind)
	}
	list("Denied Namespaced Resources:", nbl)
	keys := "<none>"
	if len(p.Spec.SignatureKeys) > 0 { //nolint:staticcheck // SA1019: the upstream flags and commands manage the deprecated field
		ids := make([]string, 0, len(p.Spec.SignatureKeys)) //nolint:staticcheck // SA1019: the upstream flags and commands manage the deprecated field
		for _, k := range p.Spec.SignatureKeys {            //nolint:staticcheck // SA1019: the upstream flags and commands manage the deprecated field
			ids = append(ids, k.KeyID)
		}
		keys = strings.Join(ids, ", ")
	}
	output.Printf(w, layout, "Signature keys:", keys)
	output.Printf(w, layout, "Orphaned Resources:", formatOrphanedResources(p))
}

func newProjectEditCommand(f *cmdutil.Factory) *cobra.Command {
	return &cobra.Command{
		Use:   "edit PROJECT",
		Short: "Edit project",
		Example: `  # Edit the information on project with name PROJECT
  patrol proj edit PROJECT`,
		Args:              cmdutil.ExactArgs(1),
		ValidArgsFunction: CompleteProjects(f, 1),
		RunE: func(c *cobra.Command, args []string) error {
			name := args[0]
			client, err := f.NewClient(c.Context())
			if err != nil {
				return err
			}
			proj, err := client.GetProject(c.Context(), name)
			if err != nil {
				return err
			}
			data, err := yaml.Marshal(proj.Spec)
			if err != nil {
				return err
			}
			return f.InteractiveEdit(c.Context(), name+"-*-edit.yaml", data, func(input []byte) error {
				js, err := yaml.YAMLToJSON(input)
				if err != nil {
					return fmt.Errorf("error converting YAML to JSON: %w", err)
				}
				var spec v1alpha1.AppProjectSpec
				if err := json.Unmarshal(js, &spec); err != nil {
					return fmt.Errorf("error unmarshaling input into project spec: %w", err)
				}
				cur, err := client.GetProject(c.Context(), name)
				if err != nil {
					return fmt.Errorf("could not get project by project name: %w", err)
				}
				cur.Spec = spec
				if _, err := client.UpdateProject(c.Context(), cur); err != nil {
					return fmt.Errorf("failed to update project:\n%w", err)
				}
				return nil
			})
		},
	}
}

func newProjectAddDestinationServiceAccountCommand(f *cmdutil.Factory) *cobra.Command {
	var saNamespace string
	c := &cobra.Command{
		Use:   "add-destination-service-account PROJECT SERVER NAMESPACE SERVICE_ACCOUNT",
		Short: "Add project destination's default service account",
		Example: `  # Add project destination service account (SERVICE_ACCOUNT) for a server URL (SERVER) in the specified namespace (NAMESPACE) on the project with name PROJECT
  patrol proj add-destination-service-account PROJECT SERVER NAMESPACE SERVICE_ACCOUNT

  # Add project destination service account (SERVICE_ACCOUNT) from a different namespace
  patrol proj add-destination-service-account PROJECT SERVER NAMESPACE SERVICE_ACCOUNT --service-account-namespace OTHER_NAMESPACE`,
		Args:              cmdutil.ExactArgs(4),
		ValidArgsFunction: CompleteProjects(f, 1),
		RunE: func(c *cobra.Command, args []string) error {
			server, ns, sa := args[1], args[2], args[3]
			if strings.Contains(saNamespace, "*") {
				return cmdutil.Fatalf("service-account-namespace for DestinationServiceAccount must not contain wildcards")
			}
			if strings.Contains(sa, "*") {
				return cmdutil.Fatalf("ServiceAccount for DestinationServiceAccount must not contain wildcards")
			}
			dsa := v1alpha1.ApplicationDestinationServiceAccount{Server: server, Namespace: ns, DefaultServiceAccount: sa}
			if saNamespace != "" {
				dsa.DefaultServiceAccount = saNamespace + ":" + sa
			}
			return updateProject(c, f, args[0], func(p *v1alpha1.AppProject) (bool, error) {
				for _, d := range p.Spec.DestinationServiceAccounts {
					if d.Namespace == dsa.Namespace && d.Server == dsa.Server && d.DefaultServiceAccount == dsa.DefaultServiceAccount {
						return false, cmdutil.Fatalf("Specified destination service account is already defined in project")
					}
				}
				p.Spec.DestinationServiceAccounts = append(p.Spec.DestinationServiceAccounts, dsa)
				return true, nil
			})
		},
	}
	c.Flags().StringVar(&saNamespace, "service-account-namespace", "", "Use service-account-namespace as namespace where the service account is present")
	return c
}

func newProjectRemoveDestinationServiceAccountCommand(f *cmdutil.Factory) *cobra.Command {
	return &cobra.Command{
		Use:   "remove-destination-service-account PROJECT SERVER NAMESPACE SERVICE_ACCOUNT",
		Short: "Remove default destination service account from the project",
		Example: `  # Remove the destination service account (SERVICE_ACCOUNT) from the specified destination (SERVER and NAMESPACE combination) on the project with name PROJECT
  patrol proj remove-destination-service-account PROJECT SERVER NAMESPACE SERVICE_ACCOUNT`,
		Args:              cmdutil.ExactArgs(4),
		ValidArgsFunction: CompleteProjects(f, 1),
		RunE: func(c *cobra.Command, args []string) error {
			server, ns, sa := args[1], args[2], args[3]
			return updateProject(c, f, args[0], func(p *v1alpha1.AppProject) (bool, error) {
				n := len(p.Spec.DestinationServiceAccounts)
				p.Spec.DestinationServiceAccounts = slices.DeleteFunc(p.Spec.DestinationServiceAccounts, func(d v1alpha1.ApplicationDestinationServiceAccount) bool {
					return d.Namespace == ns && d.Server == server && d.DefaultServiceAccount == sa
				})
				if n == len(p.Spec.DestinationServiceAccounts) {
					return false, cmdutil.Fatalf("Specified destination service account does not exist in project")
				}
				return true, nil
			})
		},
	}
}
