package applicationsets

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/appset/generators"
	"github.com/juliosaraiva/patrol-cd/internal/appset/template"
)

// GenerateLimits bound one generation (generate and create dry run). They are enforced while
// the Applications are produced, so a generation that would exceed them stops early instead of
// being rejected after the whole output is in memory.
type GenerateLimits struct {
	// MaxParameterSets bounds the parameter sets the generators of the ApplicationSet produce,
	// every generator entry together and counted before post-selectors. It also bounds each
	// generator, a Matrix product included, while it is built.
	MaxParameterSets int
	// MaxApplications bounds the Applications rendered.
	MaxApplications int
	// MaxBytes bounds the JSON size of the rendered Applications together.
	MaxBytes int
}

// LimitError reports a generation stopped by a GenerateLimits bound (400).
type LimitError struct{ msg string }

func (e *LimitError) Error() string { return e.msg }

// Generator renders the Applications of an ApplicationSet without writing anything, within
// limits and honoring the context's deadline; *BoundedGenerator implements it.
type Generator interface {
	Generate(ctx context.Context, appSet *v1alpha1.ApplicationSet, limits GenerateLimits) ([]v1alpha1.Application, error)
}

// BoundedGenerator renders ApplicationSets for the API server's dry-run routes with the
// generators of internal/appset, enforcing GenerateLimits and the context deadline inside the
// generation: before every generator call, every parameter set and every rendered
// Application. It renders exactly what appset.GenerateApplications renders (same generators,
// template merging, templatePatch and namespace forcing) but stops at the first failure,
// which is the error appset.GenerateApplications reports.
//
// Like appset.NewAPIGenerator it does not validate projects or destinations, writes nothing,
// and runs generators in tokenRef strict mode without the SCM proxy, as upstream.
type BoundedGenerator struct {
	cfg generators.Config
}

// NewGenerator returns a BoundedGenerator. cfg.Client must read the API server's namespace
// (cfg.Namespace) and the ApplicationSet namespaces.
func NewGenerator(cfg generators.Config) *BoundedGenerator {
	cfg.SCM.TokenRefStrictMode = true
	cfg.SCM.ProxyURL, cfg.SCM.NoProxy = "", ""
	return &BoundedGenerator{cfg: cfg}
}

// maxCapturedLogBytes bounds the generator logs appended to Generate errors.
const maxCapturedLogBytes = 16 << 10

// Generate returns the Applications appSet generates. A generator or template failure is
// returned as "unable to generate Applications of ApplicationSet: <err>\n<captured logs>"; a
// limit as *LimitError; an ended context as the context's error.
func (g *BoundedGenerator) Generate(ctx context.Context, appSet *v1alpha1.ApplicationSet, limits GenerateLimits) ([]v1alpha1.Application, error) {
	if appSet == nil {
		return nil, errors.New("error creating ApplicationSets: ApplicationSets is nil in request")
	}
	buf := &boundedBuffer{max: maxCapturedLogBytes}
	log := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg := g.cfg
	cfg.Logger, cfg.SCM.Logger = log, log
	if limits.MaxParameterSets > 0 && limits.MaxParameterSets < effectiveMaxParams(cfg.MaxParameterSets) {
		cfg.MaxParameterSets = limits.MaxParameterSets
	}
	b := &budget{limits: limits, name: appSet.Name}
	apps, err := b.generate(ctx, log, appSet, b.wrap(generators.New(cfg)))
	if err != nil {
		var le *LimitError
		if errors.As(err, &le) || (ctx.Err() != nil && errors.Is(err, ctx.Err())) {
			return nil, err
		}
		return nil, fmt.Errorf("unable to generate Applications of ApplicationSet: %w\n%s", err, buf.String())
	}
	return apps, nil
}

// effectiveMaxParams is the per-generator bound generators.Config.MaxParameterSets stands for.
func effectiveMaxParams(n int) int {
	switch {
	case n < 0:
		return int(^uint(0) >> 1)
	case n == 0:
		return generators.DefaultMaxParameterSets
	}
	return n
}

// budget accounts one generation against its limits. It is used by one goroutine.
type budget struct {
	limits GenerateLimits
	name   string
	params int
	apps   int
	bytes  int
	// err is the first limit or context error seen by a wrapped generator.
	err error
}

func (b *budget) fail(err error) error {
	if b.err == nil {
		b.err = err
	}
	return err
}

// addParams charges n parameter sets.
func (b *budget) addParams(n int) error {
	b.params += n
	if m := b.limits.MaxParameterSets; m > 0 && b.params > m {
		return b.fail(&LimitError{msg: fmt.Sprintf("the generators of ApplicationSet %s produce more than %d parameter sets, the limit of the API", b.name, m)})
	}
	return nil
}

// addApplication charges one rendered Application.
func (b *budget) addApplication(app *v1alpha1.Application) error {
	b.apps++
	if m := b.limits.MaxApplications; m > 0 && b.apps > m {
		return b.fail(&LimitError{msg: fmt.Sprintf("ApplicationSet %s generates more than %d Applications, the limit of the API", b.name, m)})
	}
	if m := b.limits.MaxBytes; m > 0 {
		raw, err := json.Marshal(app)
		if err != nil {
			return fmt.Errorf("encode generated Application: %w", err)
		}
		b.bytes += len(raw)
		if b.bytes > m {
			return b.fail(&LimitError{msg: fmt.Sprintf("the Applications generated by ApplicationSet %s exceed the response limit of %d bytes", b.name, m)})
		}
	}
	return nil
}

// wrap returns set with every top-level generator charged against the budget: the context is
// checked before each call and the parameter sets it returns are counted. Matrix and Merge
// children are bounded per call by generators.Config.MaxParameterSets.
func (b *budget) wrap(set generators.Set) generators.Set {
	kinds := []generators.Kind{
		generators.KindList, generators.KindClusters, generators.KindGit, generators.KindSCMProvider,
		generators.KindClusterDecisionResource, generators.KindPullRequest, generators.KindMatrix,
		generators.KindMerge, generators.KindPlugin,
	}
	out := make(generators.Set, len(set))
	for k, g := range set {
		out[k] = g // entries that are not generator kinds (the Set's own settings) are kept as they are
	}
	for _, k := range kinds {
		if g, ok := set[k]; ok && g != nil {
			out[k] = &budgetedGenerator{inner: g, b: b}
		}
	}
	return out
}

// budgetedGenerator charges a generator's calls against a budget.
type budgetedGenerator struct {
	inner generators.Generator
	b     *budget
}

func (g *budgetedGenerator) GenerateParams(ctx context.Context, gen *v1alpha1.ApplicationSetGenerator, appSet *v1alpha1.ApplicationSet) ([]map[string]any, error) {
	if g.b.err != nil {
		return nil, g.b.err
	}
	if err := ctx.Err(); err != nil {
		return nil, g.b.fail(err)
	}
	params, err := g.inner.GenerateParams(ctx, gen, appSet)
	if err != nil {
		return nil, err
	}
	if err := g.b.addParams(len(params)); err != nil {
		return nil, err
	}
	return params, nil
}

func (g *budgetedGenerator) RequeueAfter(gen *v1alpha1.ApplicationSetGenerator) time.Duration {
	return g.inner.RequeueAfter(gen)
}

func (g *budgetedGenerator) Template(gen *v1alpha1.ApplicationSetGenerator) *v1alpha1.ApplicationSetTemplate {
	return g.inner.Template(gen)
}

// generate mirrors appset.GenerateApplications: for every generator entry in order and every
// generator of the entry, every parameter set renders the generator's effective template, then
// templatePatch, and the Application namespace is forced to the ApplicationSet's. It stops at
// the first failure, limit or ended context.
func (b *budget) generate(ctx context.Context, log *slog.Logger, appSet *v1alpha1.ApplicationSet, set generators.Set) ([]v1alpha1.Application, error) {
	opts := generators.TemplateOptions(appSet)
	res := []v1alpha1.Application{}
	for i, entry := range appSet.Spec.Generators {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		results, err := set.Transform(ctx, entry, appSet.Spec.Template, appSet, nil)
		if b.err != nil {
			return nil, b.err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err != nil {
			log.ErrorContext(ctx, "error generating application from params", slog.Int("generator", i), slog.Any("error", err))
			return nil, err
		}
		for _, r := range results {
			tmpl := template.ApplicationFromTemplate(r.Template)
			for _, params := range r.Params {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				app, err := template.RenderApplication(tmpl, appSet.Spec.SyncPolicy, params, opts)
				if err != nil {
					log.ErrorContext(ctx, "error rendering application from params", slog.Int("generator", i), slog.Any("error", err))
					return nil, err
				}
				if appSet.Spec.TemplatePatch != nil {
					app, err = template.ApplyTemplatePatch(app, *appSet.Spec.TemplatePatch, params, opts)
					if err != nil {
						log.ErrorContext(ctx, "error applying templatePatch", slog.Int("generator", i), slog.Any("error", err))
						return nil, err
					}
				}
				// The namespace of a generated Application is always the ApplicationSet's.
				app.Namespace = appSet.Namespace
				if err := b.addApplication(app); err != nil {
					return nil, err
				}
				res = append(res, *app)
			}
		}
	}
	return res, nil
}

// boundedBuffer keeps the first max bytes written to it (concurrency-safe: generators may log
// from their own goroutines).
type boundedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
	max int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if room := b.max - b.buf.Len(); room > 0 {
		b.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

func (b *boundedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
