package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"strings"

	jsonpatch "github.com/evanphx/json-patch/v5"
	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

type createOptions struct {
	app            appOptions
	fileURL        string
	name           string
	upsert         bool
	labels         []string
	annotations    []string
	setFinalizer   bool
	appNamespace   string
	applySpecPatch string
}

func newCreateCommand(e *env) *cobra.Command {
	o := &createOptions{}
	c := &cobra.Command{
		Use:   "create APPNAME",
		Short: "Create an application",
		Example: `  # Create a directory app
  patrol app create guestbook --repo https://github.com/argoproj/argocd-example-apps.git --path guestbook --dest-namespace default --dest-server https://kubernetes.default.svc --directory-recurse

  # Create a Jsonnet app
  patrol app create jsonnet-guestbook --repo https://github.com/argoproj/argocd-example-apps.git --path jsonnet-guestbook --dest-namespace default --dest-server https://kubernetes.default.svc --jsonnet-ext-var-str replicas=2

  # Create a Helm app
  patrol app create helm-guestbook --repo https://github.com/argoproj/argocd-example-apps.git --path helm-guestbook --dest-namespace default --dest-server https://kubernetes.default.svc --helm-set replicaCount=2

  # Create a Helm app from a Helm repo
  patrol app create nginx-ingress --repo https://charts.helm.sh/stable --helm-chart nginx-ingress --revision 1.24.3 --dest-namespace default --dest-server https://kubernetes.default.svc

  # Create a Kustomize app
  patrol app create kustomize-guestbook --repo https://github.com/argoproj/argocd-example-apps.git --path kustomize-guestbook --dest-namespace default --dest-server https://kubernetes.default.svc --kustomize-image quay.io/argoprojlabs/argocd-e2e-container:0.1

  # Create a MultiSource app while yaml file contains an application with multiple sources
  patrol app create guestbook --file <path-to-yaml-file>

  # Create a app using a custom tool:
  patrol app create kasane --repo https://github.com/argoproj/argocd-example-apps.git --path plugins/kasane --dest-namespace default --dest-server https://kubernetes.default.svc --config-management-plugin kasane`,
		RunE: func(c *cobra.Command, args []string) error { return runCreate(c, e, o, args) },
	}
	fs := c.Flags()
	fs.StringVar(&o.name, "name", "", "A name for the app, ignored if a file is set (DEPRECATED)")
	fs.BoolVar(&o.upsert, "upsert", false, "Allows to override application with the same name even if supplied application spec is different from existing spec")
	fs.StringVarP(&o.fileURL, "file", "f", "", "Filename or URL to Kubernetes manifests for the app")
	fs.StringArrayVarP(&o.labels, "label", "l", []string{}, "Labels to apply to the app")
	fs.StringArrayVar(&o.annotations, "annotations", []string{}, "Set metadata annotations (e.g. example=value)")
	fs.BoolVar(&o.setFinalizer, "set-finalizer", false, "Sets deletion finalizer on the application, application resources will be cascaded on deletion")
	fs.StringVarP(&o.appNamespace, "app-namespace", "N", "", "Namespace where the application will be created in")
	fs.StringVar(&o.applySpecPatch, "apply-spec-patch", "", "Filename or URL of a JSON merge patch (YAML or JSON) applied to the spec of every constructed application before it is created")
	_ = fs.SetAnnotation("file", cobra.BashCompFilenameExt, []string{"json", "yaml", "yml"})
	addAppFlags(fs, &o.app)
	return c
}

func runCreate(c *cobra.Command, e *env, o *createOptions, args []string) error {
	ctx := cmdContext(c)
	if len(args) > 1 {
		return usage(c, args)
	}
	apps, err := constructApps(ctx, e, c, o, args)
	if err != nil {
		return err
	}
	var specPatch []byte
	if o.applySpecPatch != "" {
		if specPatch, err = loadMergePatch(ctx, e, o.applySpecPatch); err != nil {
			return err
		}
	}
	cl, err := e.client(ctx)
	if err != nil {
		return err
	}
	for _, app := range apps {
		if app.Name == "" {
			return usage(c, args)
		}
		if o.appNamespace != "" {
			app.Namespace = o.appNamespace
		}
		if o.setFinalizer {
			app.Finalizers = append(app.Finalizers, v1alpha1.FinalizerResources)
		}
		if specPatch != nil {
			if err := applySpecPatch(app, specPatch); err != nil {
				return err
			}
		}
		existing, err := cl.GetApplication(ctx, &apiclient.ApplicationGetRequest{Name: app.Name, AppNamespace: app.Namespace})
		if err != nil {
			// The API answers 403 for a missing Application (no existence oracle).
			if !isNotFoundOrDenied(err) {
				return err
			}
			existing = nil
		}
		upsert, validate := o.upsert, o.app.validate
		created, err := cl.CreateApplication(ctx, &apiclient.ApplicationCreateRequest{Application: app, Upsert: &upsert, Validate: &validate})
		if err != nil {
			return err
		}
		action := "updated"
		switch {
		case existing == nil:
			action = "created"
		case !appChanged(existing, created, o.upsert):
			action = "unchanged"
		}
		_, _ = fmt.Fprintf(c.OutOrStdout(), "application '%s' %s\n", created.Name, action)
	}
	return nil
}

// appChanged reports whether an upsert changed the Application (upstream hasAppChanged).
func appChanged(req, res *v1alpha1.Application, upsert bool) bool {
	if !upsert {
		return false
	}
	if req.Spec.Project == "" {
		req.Spec.Project = v1alpha1.DefaultAppProjectName
	}
	norm := func(m map[string]string) map[string]string {
		if len(m) == 0 {
			return nil
		}
		return m
	}
	normS := func(s []string) []string {
		if len(s) == 0 {
			return nil
		}
		return s
	}
	same := reflect.DeepEqual(res.Spec, req.Spec) &&
		reflect.DeepEqual(norm(res.Labels), norm(req.Labels)) &&
		reflect.DeepEqual(norm(res.Annotations), norm(req.Annotations)) &&
		reflect.DeepEqual(normS(res.Finalizers), normS(req.Finalizers))
	return !same
}

// constructApps builds the Applications to create from -f (file, URL or "-" for stdin) or
// from the name argument and flags (upstream cmdutil.ConstructApps).
func constructApps(ctx context.Context, e *env, c *cobra.Command, o *createOptions, args []string) ([]*v1alpha1.Application, error) {
	fs := c.Flags()
	switch {
	case o.fileURL == "-":
		data, err := readBounded(c.InOrStdin(), "standard input")
		if err != nil {
			return nil, err
		}
		apps, err := parseApplications(data)
		if err != nil {
			return nil, fmt.Errorf("unable to read manifest from stdin: %w", err)
		}
		return apps, nil
	case o.fileURL != "":
		data, err := e.readFileOrURL(ctx, o.fileURL)
		if err != nil {
			return nil, err
		}
		apps, err := parseApplications(data)
		if err != nil {
			return nil, err
		}
		for _, app := range apps {
			if len(args) == 1 && args[0] != app.Name {
				return nil, fmt.Errorf("app name '%s' does not match app spec metadata.name '%s'", args[0], app.Name)
			}
			if o.name != "" && o.name != app.Name {
				app.Name = o.name
			}
			if app.Name == "" {
				return nil, errors.New("app.Name is empty. --name argument can be used to provide app.Name")
			}
			if err := mergeLabels(app, o.labels); err != nil {
				return nil, err
			}
			setAnnotations(app, o.annotations)
			// Flags do not override Applications with multiple sources.
			if !app.Spec.HasMultipleSources() {
				m := &specMapper{ctx: ctx, e: e, fs: fs, opts: &o.app}
				if _, err := m.apply(&app.Spec, 0); err != nil {
					return nil, err
				}
				if err := setParameterOverrides(app, o.app.parameters, 0); err != nil {
					return nil, err
				}
			}
		}
		return apps, nil
	}
	name := o.name
	if len(args) == 1 {
		if name != "" && name != args[0] {
			return nil, fmt.Errorf("--name argument '%s' does not match app name %s", name, args[0])
		}
		name = args[0]
	}
	appName, appNs := parseQualifiedName(name, "")
	app := &v1alpha1.Application{
		TypeMeta:   metav1.TypeMeta{Kind: "Application", APIVersion: v1alpha1.GroupVersion.String()},
		ObjectMeta: metav1.ObjectMeta{Name: appName, Namespace: appNs},
	}
	m := &specMapper{ctx: ctx, e: e, fs: fs, opts: &o.app}
	if _, err := m.apply(&app.Spec, 0); err != nil {
		return nil, err
	}
	if err := setParameterOverrides(app, o.app.parameters, 0); err != nil {
		return nil, err
	}
	if err := mergeLabels(app, o.labels); err != nil {
		return nil, err
	}
	setAnnotations(app, o.annotations)
	return []*v1alpha1.Application{app}, nil
}

// parseApplications decodes YAML or JSON documents (separated by ---) into Applications.
// Unknown fields are rejected so typos do not silently disappear.
func parseApplications(data []byte) ([]*v1alpha1.Application, error) {
	docs, err := splitYAMLDocuments(data)
	if err != nil {
		return nil, err
	}
	apps := make([]*v1alpha1.Application, 0, len(docs))
	for _, doc := range docs {
		var app v1alpha1.Application
		if err := decodeStrict(doc, &app); err != nil {
			return nil, fmt.Errorf("error unmarshaling application: %w", err)
		}
		apps = append(apps, &app)
	}
	return apps, nil
}

// mergeLabels adds -l key=value labels.
func mergeLabels(app *v1alpha1.Application, labels []string) error {
	parsed, err := parseKeyValues(labels)
	if err != nil {
		return err
	}
	merged := map[string]string{}
	maps.Copy(merged, app.Labels)
	maps.Copy(merged, parsed)
	app.Labels = merged
	return nil
}

// setAnnotations adds --annotations key=value (a bare key gets an empty value).
func setAnnotations(app *v1alpha1.Application, annotations []string) {
	if len(annotations) > 0 && app.Annotations == nil {
		app.Annotations = map[string]string{}
	}
	for _, a := range annotations {
		k, v, _ := strings.Cut(a, "=")
		app.Annotations[k] = v
	}
}

// loadMergePatch reads a YAML or JSON merge patch from a file or URL.
func loadMergePatch(ctx context.Context, e *env, pathOrURL string) ([]byte, error) {
	data, err := e.readFileOrURL(ctx, pathOrURL)
	if err != nil {
		return nil, err
	}
	js, err := yaml.YAMLToJSON(data)
	if err != nil {
		return nil, fmt.Errorf("invalid spec patch: %w", err)
	}
	var obj map[string]any
	if err := json.Unmarshal(js, &obj); err != nil {
		return nil, fmt.Errorf("invalid spec patch: must be an object: %w", err)
	}
	return js, nil
}

// applySpecPatch applies a JSON merge patch to the spec of app.
func applySpecPatch(app *v1alpha1.Application, patch []byte) error {
	spec, err := json.Marshal(app.Spec)
	if err != nil {
		return fmt.Errorf("encode spec: %w", err)
	}
	patched, err := jsonpatch.MergePatch(spec, patch)
	if err != nil {
		return fmt.Errorf("apply spec patch: %w", err)
	}
	var out v1alpha1.ApplicationSpec
	if err := json.Unmarshal(patched, &out); err != nil {
		return fmt.Errorf("apply spec patch: %w", err)
	}
	app.Spec = out
	return nil
}
