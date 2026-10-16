package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/helm"
	"github.com/juliosaraiva/patrol-cd/internal/render"
	"github.com/juliosaraiva/patrol-cd/internal/render/jsonnet"
	reposerver "github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
	"github.com/juliosaraiva/patrol-cd/internal/tracking"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// localRenderRequest describes a client-side rendering of a local directory with the
// Application's source settings (upstream getLocalObjectsString).
type localRenderRequest struct {
	// Dir is the application directory (--local).
	Dir string
	// RepoRoot is the repository root that confines the rendering (--local-repo-root).
	RepoRoot string
	App      *v1alpha1.Application
	Settings *apiclient.Settings
	// KubeVersion and APIVersions describe the destination cluster.
	KubeVersion string
	APIVersions []string
}

// localRenderer renders local sources on the client.
type localRenderer interface {
	Render(ctx context.Context, req localRenderRequest) ([]string, error)
}

// toolRenderer renders with internal/render and the helm/kustomize binaries on the PATH
// (the same rendering code as the repo-server). Config Management Plugins and Helm chart
// repository sources cannot be rendered locally; use --server-side-generate.
type toolRenderer struct{}

func (toolRenderer) Render(ctx context.Context, req localRenderRequest) ([]string, error) {
	src := firstSource(&req.App.Spec)
	if src.Plugin != nil {
		return nil, errors.New("local rendering does not work with config management plugins: use --server-side-generate")
	}
	if src.Chart != "" {
		return nil, errors.New("local rendering of a Helm chart repository source is not supported: use --server-side-generate")
	}
	root, err := filepath.Abs(req.RepoRoot)
	if err != nil {
		return nil, err
	}
	dir, err := filepath.Abs(req.Dir)
	if err != nil {
		return nil, err
	}
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("local path %s is not inside the repository root %s", req.Dir, req.RepoRoot)
	}
	work, err := os.MkdirTemp("", "patrol-render-")
	if err != nil {
		return nil, fmt.Errorf("create work directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(work) }()
	r, err := render.New(render.Config{
		Helm:    &helm.Client{WorkDir: filepath.Join(work, "helm")},
		WorkDir: work,
		Jsonnet: jsonnet.Runner{NoRlimits: true},
	})
	if err != nil {
		return nil, err
	}
	st := req.Settings
	if st == nil {
		st = &apiclient.Settings{}
	}
	tracker := tracking.New(tracking.Config{InstanceLabelKey: st.AppLabelKey, ControlPlaneNamespace: st.ControllerNamespace})
	in := render.Input{
		RootDir:     root,
		AppPath:     rel,
		Source:      src,
		Revision:    src.TargetRevision,
		Repo:        reposerver.Repository{URL: src.RepoURL},
		AppName:     tracking.InstanceName(req.App.Name, req.App.Namespace, st.ControllerNamespace),
		ProjectName: req.App.Spec.GetProject(),
		Namespace:   req.App.Spec.Destination.Namespace,
		KubeVersion: req.KubeVersion,
		APIVersions: req.APIVersions,
		Tracking: &render.Tracking{
			Tracker:        tracker,
			Method:         tracking.EffectiveMethod(v1alpha1.TrackingMethod(st.TrackingMethod)),
			InstallationID: st.InstallationID,
		},
		ManifestGeneratePaths: req.App.Annotations[v1alpha1.AnnotationKeyManifestGeneratePaths],
	}
	if st.KustomizeOptions != nil {
		in.KustomizeOptions = reposerver.KustomizeOptions{BuildOptions: st.KustomizeOptions.BuildOptions}
	}
	res, err := r.Generate(ctx, in)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(res.Manifests))
	for _, obj := range res.Manifests {
		data, err := json.Marshal(obj.Object)
		if err != nil {
			return nil, fmt.Errorf("encode manifest: %w", err)
		}
		out = append(out, string(data))
	}
	return out, nil
}

// renderLocal renders --local for app with the destination cluster's version information.
func (e *env) renderLocal(ctx context.Context, cl Client, app *v1alpha1.Application, settings *apiclient.Settings, dir, repoRoot string) ([]string, error) {
	req := localRenderRequest{Dir: dir, RepoRoot: repoRoot, App: app, Settings: settings}
	id := apiclient.ClusterByServer(app.Spec.Destination.Server)
	if app.Spec.Destination.Server == "" {
		id = apiclient.ClusterByName(app.Spec.Destination.Name)
	}
	cluster, err := cl.GetCluster(ctx, id)
	if err != nil {
		return nil, err
	}
	req.KubeVersion = cluster.Status.ServerVersion
	req.APIVersions = cluster.Status.Info.APIVersions
	return e.renderer.Render(ctx, req)
}
