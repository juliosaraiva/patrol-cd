package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/diff"
)

func newAppCommand(f *Factory, kube *kubeOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "app",
		Short: "Manage applications configuration",
		Example: `
# Compare results of two reconciliations and print diff
patrol admin app diff-reconcile-results PATH1 PATH2

# Generate declarative config for an application
patrol admin app generate-spec APPNAME

# Reconcile all applications and store reconciliation summary in the specified file
patrol admin app get-reconcile-results PATH
`,
		RunE: func(c *cobra.Command, _ []string) error { return c.Help() },
	}
	cmd.AddCommand(newAppGenerateSpecCommand(f), newGetReconcileResultsCommand(f, kube), newDiffReconcileResultsCommand(f))
	return cmd
}

// outWriter returns stdout, or (inline) a new file at path after renaming the original to
// path+".back" (upstream getOutWriter).
func outWriter(inline bool, path string, stdout io.Writer) (io.Writer, func() error, error) {
	if !inline {
		return stdout, func() error { return nil }, nil
	}
	if path == "" {
		return nil, nil, errors.New("the file path must be specified using flag '--file'")
	}
	st, err := os.Stat(path)
	if err != nil {
		return nil, nil, err
	}
	if err := os.Rename(path, path+".back"); err != nil {
		return nil, nil, err
	}
	out, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, st.Mode().Perm())
	if err != nil {
		return nil, nil, err
	}
	return out, out.Close, nil
}

func newAppGenerateSpecCommand(f *Factory) *cobra.Command {
	var (
		o            appOptions
		fileURL      string
		appName      string
		labels       []string
		annotations  []string
		output       string
		inline       bool
		setFinalizer bool
	)
	cmd := &cobra.Command{
		Use:   "generate-spec APPNAME",
		Short: "Generate declarative config for an application",
		Example: `
	# Generate declarative config for a directory app
	patrol admin app generate-spec guestbook --repo https://github.com/argoproj/argocd-example-apps.git --path guestbook --dest-namespace default --dest-server https://kubernetes.default.svc --directory-recurse

	# Generate declarative config for a Jsonnet app
	patrol admin app generate-spec jsonnet-guestbook --repo https://github.com/argoproj/argocd-example-apps.git --path jsonnet-guestbook --dest-namespace default --dest-server https://kubernetes.default.svc --jsonnet-ext-var-str replicas=2

	# Generate declarative config for a Helm app
	patrol admin app generate-spec helm-guestbook --repo https://github.com/argoproj/argocd-example-apps.git --path helm-guestbook --dest-namespace default --dest-server https://kubernetes.default.svc --helm-set replicaCount=2

	# Generate declarative config for a Helm app from a Helm repo
	patrol admin app generate-spec nginx-ingress --repo https://charts.helm.sh/stable --helm-chart nginx-ingress --revision 1.24.3 --dest-namespace default --dest-server https://kubernetes.default.svc

	# Generate declarative config for a Kustomize app
	patrol admin app generate-spec kustomize-guestbook --repo https://github.com/argoproj/argocd-example-apps.git --path kustomize-guestbook --dest-namespace default --dest-server https://kubernetes.default.svc --kustomize-image quay.io/argoprojlabs/argocd-e2e-container:0.1

	# Generate declarative config for a app using a custom tool:
	patrol admin app generate-spec kasane --repo https://github.com/argoproj/argocd-example-apps.git --path plugins/kasane --dest-namespace default --dest-server https://kubernetes.default.svc --config-management-plugin kasane
`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			apps, err := constructApps(c.Context(), f.in(), fileURL, appName, labels, annotations, args, &o, c.Flags())
			if err != nil {
				return err
			}
			if len(apps) > 1 {
				return errors.New("failed to generate spec, more than one application is not supported")
			}
			if len(apps) == 0 || apps[0].Name == "" {
				_ = c.Help()
				return exitWith(1, nil)
			}
			app := apps[0]
			if setFinalizer {
				app.Finalizers = append(app.Finalizers, v1alpha1.FinalizerResources)
			}
			if app.APIVersion == "" {
				app.APIVersion, app.Kind = v1alpha1.GroupVersion.String(), "Application"
			}
			w, closeOut, err := outWriter(inline, fileURL, f.out())
			if err != nil {
				return err
			}
			perr := printResources(w, output, app)
			return errors.Join(perr, closeOut())
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&appName, "name", "", "A name for the app, ignored if a file is set (DEPRECATED)")
	fl.StringVarP(&fileURL, "file", "f", "", "Filename or URL to Kubernetes manifests for the app")
	fl.StringArrayVarP(&labels, "label", "l", []string{}, "Labels to apply to the app")
	fl.StringArrayVar(&annotations, "annotations", []string{}, "Set metadata annotations (e.g. example=value)")
	fl.StringVarP(&output, "output", "o", formatYAML, "Output format. One of: json|yaml")
	fl.BoolVarP(&inline, "inline", "i", false, "If set then generated resource is written back to the file specified in --file flag")
	fl.BoolVar(&setFinalizer, "set-finalizer", false, "Sets deletion finalizer on the application, application resources will be cascaded on deletion")
	_ = fl.SetAnnotation("file", cobra.BashCompFilenameExt, []string{"json", "yaml", "yml"})
	addAppFlags(cmd, &o)
	return cmd
}

// appReconcileResult is one Application of a reconciliation summary.
type appReconcileResult struct {
	Name       string                    `json:"name"`
	Health     v1alpha1.HealthStatusCode `json:"health"`
	Sync       *v1alpha1.SyncStatus      `json:"sync"`
	Conditions []metav1.Condition        `json:"conditions"`
}

// reconcileResults is the summary written by get-reconcile-results.
type reconcileResults struct {
	Applications []appReconcileResult `json:"applications"`
}

func newGetReconcileResultsCommand(f *Factory, kube *kubeOptions) *cobra.Command {
	var (
		selector       string
		output         string
		refresh        bool
		serverSideDiff bool
		jqTimeout      time.Duration
		repo           repoServerOptions
	)
	cmd := &cobra.Command{
		Use:   "get-reconcile-results PATH",
		Short: "Reconcile all applications and stores reconciliation summary in the specified file.",
		Long: `Write the health, sync status and conditions of every Application of the namespace (label
selector --l) to PATH. Without --refresh the results are the stored status; with --refresh
the applications are compared again locally (repo-server at --repo-server, live state read
with the kubeconfig credentials) without the controller.`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			ctx := c.Context()
			if output != formatYAML && output != formatJSON {
				return fmt.Errorf("format %s is not supported", output)
			}
			kc, err := kube.get(nil)
			if err != nil {
				return err
			}
			var results []appReconcileResult
			if refresh {
				rc, err := repo.client()
				if err != nil {
					return err
				}
				if rc == nil {
					return errors.New("--refresh requires --repo-server (and --repo-server-token-file)")
				}
				results, err = reconcileApplications(ctx, f, kc, rc, selector, serverSideDiff, jqTimeout)
				if err != nil {
					return err
				}
			} else if results, err = storedReconcileResults(ctx, kc, selector); err != nil {
				return err
			}
			return saveReconcileResults(args[0], output, reconcileResults{Applications: results})
		},
	}
	fl := cmd.Flags()
	repo.bind(fl, "repo-server", "")
	fl.StringVar(&selector, "l", "", "Label selector")
	fl.StringVar(&output, "o", formatYAML, "Output format (yaml|json)")
	fl.BoolVar(&refresh, "refresh", false, "If set to true then recalculates apps reconciliation")
	fl.BoolVar(&serverSideDiff, "server-side-diff", false, "If set to \"true\" will use server-side diff while comparing resources. Default (\"false\")")
	fl.DurationVar(&jqTimeout, "ignore-normalizer-jq-execution-timeout", diff.DefaultJQTimeout, "Set ignore normalizer JQ execution timeout")
	return cmd
}

func listApplications(ctx context.Context, kc *kubeClients, selector string) ([]v1alpha1.Application, error) {
	opts := []client.ListOption{client.InNamespace(kc.namespace)}
	if selector != "" {
		sel, err := metav1.ParseToLabelSelector(selector)
		if err != nil {
			return nil, fmt.Errorf("invalid label selector: %w", err)
		}
		s, err := metav1.LabelSelectorAsSelector(sel)
		if err != nil {
			return nil, fmt.Errorf("invalid label selector: %w", err)
		}
		opts = append(opts, client.MatchingLabelsSelector{Selector: s})
	}
	var list v1alpha1.ApplicationList
	if err := kc.ctrl.List(ctx, &list, opts...); err != nil {
		return nil, fmt.Errorf("error listing namespaced apps: %w", err)
	}
	return list.Items, nil
}

func storedReconcileResults(ctx context.Context, kc *kubeClients, selector string) ([]appReconcileResult, error) {
	apps, err := listApplications(ctx, kc, selector)
	if err != nil {
		return nil, err
	}
	out := make([]appReconcileResult, 0, len(apps))
	for _, app := range apps {
		sync := app.Status.Sync
		out = append(out, appReconcileResult{Name: app.Name, Health: app.Status.Health.Status, Sync: &sync, Conditions: app.Status.Conditions})
	}
	return out, nil
}

func saveReconcileResults(path, output string, results reconcileResults) error {
	var data []byte
	var err error
	if output == formatJSON {
		data, err = json.Marshal(results)
	} else {
		data, err = yaml.Marshal(results)
	}
	if err != nil {
		return fmt.Errorf("error marshalling %s: %w", output, err)
	}
	return os.WriteFile(path, data, 0o644) //nolint:gosec // reconciliation summaries hold no credentials (upstream mode)
}

func newDiffReconcileResultsCommand(f *Factory) *cobra.Command {
	return &cobra.Command{
		Use:   "diff-reconcile-results PATH1 PATH2",
		Short: "Compare results of two reconciliations and print diff.",
		Args:  cobra.ExactArgs(2),
		RunE: func(c *cobra.Command, args []string) error {
			var res1, res2 reconcileResults
			for i, p := range args {
				data, err := readFileLimited(p)
				if err != nil {
					return err
				}
				into := &res1
				if i == 1 {
					into = &res2
				}
				if err := yaml.Unmarshal(data, into); err != nil {
					return fmt.Errorf("error parsing %s: %w", p, err)
				}
			}
			return diffReconcileResults(f.out(), res1, res2)
		},
	}
}

type diffPair struct {
	name          string
	first, second *unstructured.Unstructured
}

func resultObject(r appReconcileResult) (*unstructured.Unstructured, error) {
	m, err := toMap(r)
	if err != nil {
		return nil, err
	}
	return &unstructured.Unstructured{Object: m}, nil
}

// diffReconcileResults prints, per application name (sorted), the name and the diff of its
// two entries; applications on one side only are diffed against nothing.
func diffReconcileResults(w io.Writer, res1, res2 reconcileResults) error {
	m2 := map[string]appReconcileResult{}
	for _, r := range res2.Applications {
		m2[r.Name] = r
	}
	var pairs []diffPair
	seen := map[string]bool{}
	for _, r := range res1.Applications {
		first, err := resultObject(r)
		if err != nil {
			return err
		}
		p := diffPair{name: r.Name, first: first}
		if s, ok := m2[r.Name]; ok {
			if p.second, err = resultObject(s); err != nil {
				return err
			}
		}
		seen[r.Name] = true
		pairs = append(pairs, p)
	}
	for _, r := range res2.Applications {
		if seen[r.Name] {
			continue
		}
		second, err := resultObject(r)
		if err != nil {
			return err
		}
		pairs = append(pairs, diffPair{name: r.Name, second: second})
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].name < pairs[j].name })
	for _, p := range pairs {
		if _, err := fmt.Fprintln(w, p.name); err != nil {
			return err
		}
		if err := printObjectDiff(w, p.name, p.first, p.second); err != nil {
			return err
		}
	}
	return nil
}
