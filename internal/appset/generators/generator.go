// Package generators implements the ApplicationSet generators (List, Clusters, Git, SCM
// Provider, Pull Request, Cluster Decision Resource, Plugin, Matrix and Merge), the generator
// post-selector, generator `values`, and the transformation of a generator entry into parameter
// sets plus its effective template.
//
// Behavior follows Argo CD v3.5.5 (applicationset/generators, applicationset/services); the
// implementation is independent. Parameter shapes, error texts and requeue defaults match
// upstream; differences are listed in docs/architecture/components/appset.md.
package generators

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/appset/template"
)

// Generator produces parameter sets for one generator type.
type Generator interface {
	// GenerateParams returns the parameter sets of gen for appSet.
	GenerateParams(ctx context.Context, gen *v1alpha1.ApplicationSetGenerator, appSet *v1alpha1.ApplicationSet) ([]map[string]any, error)
	// RequeueAfter is the polling interval the generator asks for (NoRequeueAfter: none).
	RequeueAfter(gen *v1alpha1.ApplicationSetGenerator) time.Duration
	// Template returns the generator-level template override.
	Template(gen *v1alpha1.ApplicationSetGenerator) *v1alpha1.ApplicationSetTemplate
}

// Kind names a generator type (the field name in ApplicationSetGenerator).
type Kind string

// Generator kinds, in the field order of ApplicationSetGenerator.
const (
	KindList                    Kind = "List"
	KindClusters                Kind = "Clusters"
	KindGit                     Kind = "Git"
	KindSCMProvider             Kind = "SCMProvider"
	KindClusterDecisionResource Kind = "ClusterDecisionResource"
	KindPullRequest             Kind = "PullRequest"
	KindMatrix                  Kind = "Matrix"
	KindMerge                   Kind = "Merge"
	KindPlugin                  Kind = "Plugin"
)

// Requeue defaults.
const (
	// NoRequeueAfter means the generator asks for no time-based requeue.
	NoRequeueAfter time.Duration = 0
	// DefaultRequeueAfter is the default polling interval of Git and Cluster Decision Resource
	// generators (applicationsetcontroller.requeue.after).
	DefaultRequeueAfter = 3 * time.Minute
	// DefaultSCMProviderRequeueAfter is the hard-coded default of SCM Provider, Pull Request and
	// Plugin generators.
	DefaultSCMProviderRequeueAfter = 30 * time.Minute
	// DefaultPullRequestRequeueAfter is the default of the Pull Request generator.
	DefaultPullRequestRequeueAfter = 30 * time.Minute
	// DefaultPluginRequeueAfter is the default of the Plugin generator.
	DefaultPluginRequeueAfter = 30 * time.Minute
)

// Errors shared by the generators.
var (
	// ErrEmptyAppSetGenerator is returned for a generator without configuration.
	ErrEmptyAppSetGenerator = errors.New("ApplicationSet is empty")
	// ErrSCMProvidersDisabled is returned by SCM Provider and Pull Request generators when SCM
	// providers are disabled.
	ErrSCMProvidersDisabled = errors.New("scm providers are disabled")
)

// Set maps generator kinds to implementations.
type Set map[Kind]Generator

// Config holds the dependencies of the generators.
type Config struct {
	// Client reads Clusters, AppProjects, ConfigMaps, Secrets and SCMProviders (normally the
	// manager's cached client).
	Client client.Reader
	// Namespace is the control-plane namespace: Clusters, AppProjects, plugin and cluster
	// decision resource ConfigMaps, plugin token Secrets and GitHub App Secrets live there.
	Namespace string
	// Repos serves the Git generator (repo-server GetGitFiles/GetGitDirectories).
	Repos RepoService
	// Dynamic lists cluster decision resources.
	Dynamic dynamic.Interface
	// SCM configures the SCM Provider and Pull Request generators.
	SCM SCMConfig
	// Plugin configures the Plugin generator.
	Plugin PluginConfig
	// DefaultRequeueAfter overrides DefaultRequeueAfter for Git and Cluster Decision Resource
	// generators (zero means DefaultRequeueAfter).
	DefaultRequeueAfter time.Duration
	// MaxParameterSets bounds the parameter sets one generator (a Matrix product included) may
	// produce; zero means DefaultMaxParameterSets, a negative value disables the bound.
	MaxParameterSets int
	// Logger receives generator diagnostics; nil discards them.
	Logger *slog.Logger
}

func (c Config) logger() *slog.Logger {
	if c.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return c.Logger
}

// DefaultMaxParameterSets is the default of Config.MaxParameterSets. Generated parameter sets
// and Applications are held in memory by the shared controller; a Matrix of two large children
// multiplies their sizes.
const DefaultMaxParameterSets = 20_000

// ErrTooManyParameterSets is returned when a generator produces more parameter sets than
// Config.MaxParameterSets.
var ErrTooManyParameterSets = errors.New("too many parameter sets")

func (c Config) maxParameterSets() int {
	switch {
	case c.MaxParameterSets < 0:
		return int(^uint(0) >> 1)
	case c.MaxParameterSets == 0:
		return DefaultMaxParameterSets
	}
	return c.MaxParameterSets
}

func tooManyParameterSets(max int) error {
	return fmt.Errorf("%w: a generator produced more than %d parameter sets", ErrTooManyParameterSets, max)
}

func (c Config) defaultRequeueAfter() time.Duration {
	if c.DefaultRequeueAfter <= 0 {
		return DefaultRequeueAfter
	}
	return c.DefaultRequeueAfter
}

// New returns the generators available at the top level of an ApplicationSet. Matrix and Merge
// accept every terminal generator and one nested Matrix/Merge level whose own children are
// terminal only.
func New(cfg Config) Set {
	terminal := Set{
		KindList:                    &ListGenerator{},
		KindClusters:                &ClusterGenerator{cfg: cfg},
		KindGit:                     &GitGenerator{cfg: cfg},
		KindSCMProvider:             &SCMProviderGenerator{cfg: cfg},
		KindClusterDecisionResource: &DuckTypeGenerator{cfg: cfg},
		KindPullRequest:             &PullRequestGenerator{cfg: cfg},
		KindPlugin:                  &PluginGenerator{cfg: cfg},
	}
	max := cfg.maxParameterSets()
	nested := Set{}
	for k, v := range terminal {
		nested[k] = v
	}
	nested[KindMatrix] = &MatrixGenerator{children: terminal, maxParams: max}
	nested[KindMerge] = &MergeGenerator{children: terminal}

	top := Set{}
	for k, v := range terminal {
		top[k] = v
	}
	top[KindMatrix] = &MatrixGenerator{children: nested, maxParams: max}
	top[KindMerge] = &MergeGenerator{children: nested}
	top[maxParamsKey] = maxParamsMarker(max)
	nested[maxParamsKey] = maxParamsMarker(max)
	return top
}

// maxParamsKey stores the parameter set bound in a Set (it is not a generator kind: Relevant
// never returns it).
const maxParamsKey Kind = "\x00maxParameterSets"

// maxParamsMarker carries the bound; it never generates.
type maxParamsMarker int

func (maxParamsMarker) GenerateParams(context.Context, *v1alpha1.ApplicationSetGenerator, *v1alpha1.ApplicationSet) ([]map[string]any, error) {
	return nil, ErrEmptyAppSetGenerator
}
func (maxParamsMarker) RequeueAfter(*v1alpha1.ApplicationSetGenerator) time.Duration {
	return NoRequeueAfter
}
func (maxParamsMarker) Template(*v1alpha1.ApplicationSetGenerator) *v1alpha1.ApplicationSetTemplate {
	return nil
}

// maxParams is the parameter set bound of s (DefaultMaxParameterSets for a Set not built by New).
func (s Set) maxParams() int {
	if m, ok := s[maxParamsKey].(maxParamsMarker); ok {
		return int(m)
	}
	return DefaultMaxParameterSets
}

// Relevant returns the kinds set in gen in field order. The post-selector is not a generator.
func Relevant(gen *v1alpha1.ApplicationSetGenerator) []Kind {
	if gen == nil {
		return nil
	}
	var kinds []Kind
	if gen.List != nil {
		kinds = append(kinds, KindList)
	}
	if gen.Clusters != nil {
		kinds = append(kinds, KindClusters)
	}
	if gen.Git != nil {
		kinds = append(kinds, KindGit)
	}
	if gen.SCMProvider != nil {
		kinds = append(kinds, KindSCMProvider)
	}
	if gen.ClusterDecisionResource != nil {
		kinds = append(kinds, KindClusterDecisionResource)
	}
	if gen.PullRequest != nil {
		kinds = append(kinds, KindPullRequest)
	}
	if gen.Matrix != nil {
		kinds = append(kinds, KindMatrix)
	}
	if gen.Merge != nil {
		kinds = append(kinds, KindMerge)
	}
	if gen.Plugin != nil {
		kinds = append(kinds, KindPlugin)
	}
	return kinds
}

// TransformResult is the output of one generator of a generator entry.
type TransformResult struct {
	// Params are the parameter sets that passed the post-selector.
	Params []map[string]any
	// Template is the generator template merged with the base template.
	Template v1alpha1.ApplicationSetTemplate
}

// Transform runs every generator set in entry. Each generator's template is merged with base
// first; when genParams is non-empty (second child of a matrix) the entry is interpolated with
// them before generating. Parameter sets are filtered with the entry's selector. Generation does
// not stop at the first failure: every generator is attempted and the first error is returned
// together with the successful results.
func (s Set) Transform(ctx context.Context, entry v1alpha1.ApplicationSetGenerator, base v1alpha1.ApplicationSetTemplate, appSet *v1alpha1.ApplicationSet, genParams map[string]any) ([]TransformResult, error) {
	sel, err := LabelSelectorAsSelector(entry.Selector)
	if err != nil {
		return nil, fmt.Errorf("error parsing label selector: %w", err)
	}
	opts := TemplateOptions(appSet)
	var (
		res        []TransformResult
		firstError error
	)
	record := func(err error) {
		if firstError == nil {
			firstError = err
		}
	}
	interpolated := entry.DeepCopy()
	for _, kind := range Relevant(&entry) {
		g, ok := s[kind]
		if !ok || g == nil {
			record(fmt.Errorf("generator %s is not supported here", kind))
			continue
		}
		merged := template.MergeTemplate(g.Template(&entry), base)
		gctx := ctx
		if _, scoped := ctx.Value(scopeProjectKey{}).(string); !scoped {
			// Outermost entry: its effective template decides the project of every Application
			// it generates, nested Matrix/Merge children included (their templates never apply).
			gctx = context.WithValue(ctx, scopeProjectKey{}, merged.Spec.Project)
		}
		if len(genParams) != 0 {
			interpolated, err = template.InterpolateGenerator(&entry, genParams, opts)
			if err != nil {
				record(err)
				continue
			}
		}
		params, err := g.GenerateParams(gctx, interpolated, appSet)
		if err != nil {
			record(err)
			continue
		}
		if max := s.maxParams(); len(params) > max {
			record(tooManyParameterSets(max))
			continue
		}
		var filtered []map[string]any
		for _, p := range params {
			if entry.Selector == nil {
				filtered = append(filtered, p)
				continue
			}
			flat, err := FlattenParameters(p)
			if err != nil {
				record(err)
				continue
			}
			if sel.Matches(labels.Set(flat)) {
				filtered = append(filtered, p)
			}
		}
		res = append(res, TransformResult{Params: filtered, Template: merged})
	}
	return res, firstError
}

// scopeProjectKey is the context key of the project of the outermost generator entry's
// effective template (set by Transform).
type scopeProjectKey struct{}

// ScopeProject returns the project that scopes repository credentials and source integrity for
// a generator running under ctx: the spec.project of the effective template of the outermost
// generator entry being transformed (the generator-level template's project when set, else
// spec.template.spec.project), or spec.template.spec.project when the generator is called
// outside Transform.
func ScopeProject(ctx context.Context, appSet *v1alpha1.ApplicationSet) string {
	if p, ok := ctx.Value(scopeProjectKey{}).(string); ok && p != "" {
		return p
	}
	if appSet == nil {
		return ""
	}
	return appSet.Spec.Template.Spec.Project
}

// ProjectNotPermittedError reports an ApplicationSet whose namespace is not listed in the
// spec.sourceNamespaces of the project that would scope a generator's credentials.
type ProjectNotPermittedError struct {
	Namespace string
	Project   string
}

// Error implements error.
func (e *ProjectNotPermittedError) Error() string {
	return fmt.Sprintf("ApplicationSet namespace '%s' is not permitted to use project '%s'", e.Namespace, e.Project)
}

// MinRequeueAfter returns the smallest non-zero requeue interval asked for by the generators of
// entries, or NoRequeueAfter.
func (s Set) MinRequeueAfter(entries []v1alpha1.ApplicationSetGenerator) time.Duration {
	var res time.Duration
	for i := range entries {
		for _, kind := range Relevant(&entries[i]) {
			g, ok := s[kind]
			if !ok || g == nil {
				continue
			}
			d := g.RequeueAfter(&entries[i])
			if d != NoRequeueAfter && (res == 0 || d < res) {
				res = d
			}
		}
	}
	return res
}

// TemplateOptions returns the templating options of appSet.
func TemplateOptions(appSet *v1alpha1.ApplicationSet) template.Options {
	if appSet == nil {
		return template.Options{}
	}
	return template.Options{GoTemplate: appSet.Spec.GoTemplate, GoTemplateOptions: appSet.Spec.GoTemplateOptions}
}

// requeueSeconds returns the requeueAfterSeconds override or def. Negative values disable
// time-based requeueing (upstream would requeue immediately in a loop).
func requeueSeconds(s *int64, def time.Duration) time.Duration {
	if s == nil {
		return def
	}
	if *s <= 0 {
		return NoRequeueAfter
	}
	return time.Duration(*s) * time.Second
}

func appSetLogAttrs(appSet *v1alpha1.ApplicationSet) slog.Attr {
	if appSet == nil {
		return slog.String("applicationset", "")
	}
	return slog.String("applicationset", appSet.Namespace+"/"+appSet.Name)
}
