package appset

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"sort"
	"strings"
	"sync"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/appset/generators"
	"github.com/juliosaraiva/patrol-cd/internal/appset/template"
)

// LastAppliedConfigAnnotation is kubectl's client-side apply annotation, used to recover the
// names of unrecognized generators.
const LastAppliedConfigAnnotation = "kubectl.kubernetes.io/last-applied-configuration"

// ErrTemplatedProject is returned by ValidateForAPI and Projects for an ApplicationSet whose
// spec.template.spec.project, or the spec.project of a generator-level template, is templated
// (upstream API restriction, extended to generator templates).
var ErrTemplatedProject = errors.New("the API does not currently support creating ApplicationSets with templated `project` fields")

// GenerateApplications renders the Applications of appSet: for every generator entry (in order)
// and every generator of the entry, every parameter set renders the generator's effective
// template, then templatePatch, and the Application namespace is set to the ApplicationSet's.
//
// Generation does not stop at the first failure: every entry and parameter set is attempted,
// and the first error is returned with its condition reason
// (ApplicationGenerationFromParamsError or RenderTemplateParamsError) together with the
// Applications that rendered.
func GenerateApplications(ctx context.Context, log *slog.Logger, appSet *v1alpha1.ApplicationSet, set generators.Set) ([]v1alpha1.Application, string, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	var (
		res        []v1alpha1.Application
		firstError error
		reason     string
	)
	opts := generators.TemplateOptions(appSet)
	record := func(err error, r string) {
		if firstError == nil {
			firstError, reason = err, r
		}
	}
	for i, entry := range appSet.Spec.Generators {
		results, err := set.Transform(ctx, entry, appSet.Spec.Template, appSet, nil)
		if err != nil {
			log.ErrorContext(ctx, "error generating application from params",
				slog.Int("generator", i), slog.Any("error", err))
			record(err, v1alpha1.ApplicationSetReasonApplicationParamsGenerationError)
			if ctx.Err() != nil {
				return res, reason, firstError
			}
			// Transform returns the results of the generators that succeeded alongside the
			// first error; upstream discards them for the entry, and so do we.
			continue
		}
		before := len(res)
		for _, r := range results {
			tmpl := template.ApplicationFromTemplate(r.Template)
			for _, params := range r.Params {
				app, err := template.RenderApplication(tmpl, appSet.Spec.SyncPolicy, params, opts)
				if err != nil {
					log.ErrorContext(ctx, "error rendering application from params",
						slog.Int("generator", i), slog.Any("error", err))
					record(err, v1alpha1.ApplicationSetReasonRenderTemplateParamsError)
					continue
				}
				if appSet.Spec.TemplatePatch != nil {
					patched, err := template.ApplyTemplatePatch(app, *appSet.Spec.TemplatePatch, params, opts)
					if err != nil {
						log.ErrorContext(ctx, "error applying templatePatch",
							slog.Int("generator", i), slog.Any("error", err))
						record(err, v1alpha1.ApplicationSetReasonRenderTemplateParamsError)
						continue
					}
					app = patched
				}
				// The namespace of a generated Application is always the ApplicationSet's: the
				// security boundary of ApplicationSets outside the control-plane namespace.
				app.Namespace = appSet.Namespace
				res = append(res, *app)
			}
		}
		log.DebugContext(ctx, "generated applications", slog.Int("generator", i), slog.Int("count", len(res)-before))
	}
	return res, reason, firstError
}

// CheckInvalidGenerators returns an error naming the generator entries of appSet that set no
// generator (unknown keys are pruned by the schema, so their names are recovered from kubectl's
// last-applied-configuration annotation when present). The controller only logs it; the API
// rejects such ApplicationSets.
func CheckInvalidGenerators(appSet *v1alpha1.ApplicationSet) error {
	if appSet == nil {
		return nil
	}
	invalid := false
	names := map[string]bool{}
	for i := range appSet.Spec.Generators {
		if generatorIsSet(&appSet.Spec.Generators[i]) {
			continue
		}
		invalid = true
		if n := lastAppliedGeneratorName(appSet, i); n != "" {
			names[n] = true
		}
	}
	switch {
	case len(names) > 0:
		list := make([]string, 0, len(names))
		for n := range names {
			list = append(list, n)
		}
		sort.Strings(list)
		return fmt.Errorf("ApplicationSet %s contains unrecognized generators: %s", appSet.Name, strings.Join(list, ", "))
	case invalid:
		return fmt.Errorf("ApplicationSet %s contains unrecognized generators", appSet.Name)
	}
	return nil
}

// generatorIsSet reports whether any field of the generator entry (the selector included) is set.
func generatorIsSet(g *v1alpha1.ApplicationSetGenerator) bool {
	v := reflect.ValueOf(g).Elem()
	for i := 0; i < v.NumField(); i++ {
		f := v.Field(i)
		if f.Kind() == reflect.Pointer && !f.IsNil() {
			return true
		}
	}
	return false
}

// maxLastAppliedBytes bounds the annotation parsed for generator names.
const maxLastAppliedBytes = 1 << 20

func lastAppliedGeneratorName(appSet *v1alpha1.ApplicationSet, index int) string {
	raw := appSet.Annotations[LastAppliedConfigAnnotation]
	if raw == "" || len(raw) > maxLastAppliedBytes {
		return ""
	}
	var doc struct {
		Spec struct {
			Generators []map[string]json.RawMessage `json:"generators"`
		} `json:"spec"`
	}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		return ""
	}
	if index >= len(doc.Spec.Generators) {
		return ""
	}
	keys := make([]string, 0, len(doc.Spec.Generators[index]))
	for k := range doc.Spec.Generators[index] {
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		return ""
	}
	// Upstream takes the first key of a Go map iteration (random); the smallest is
	// deterministic and equal whenever the entry has a single key.
	sort.Strings(keys)
	return keys[0]
}

// ValidateForAPI applies the API server's admission checks to an ApplicationSet before Create
// and Generate: no unrecognized generators and no templated project, neither in
// spec.template nor in a generator-level template (see Projects).
func ValidateForAPI(appSet *v1alpha1.ApplicationSet) error {
	if appSet == nil {
		return errors.New("error creating ApplicationSets: ApplicationSets is nil in request")
	}
	if err := CheckInvalidGenerators(appSet); err != nil {
		return err
	}
	_, err := Projects(appSet)
	return err
}

// Projects returns the projects the Applications of appSet can be created in, sorted and
// de-duplicated, for the API server's RBAC checks (every one must be authorized):
// spec.template.spec.project (what upstream checks) and the spec.project of every
// generator-level template that applies, which overrides it when templates are merged: the
// templates of top-level generators, Matrix and Merge included (the templates of Matrix and
// Merge children are never applied; templatePatch cannot change the project). An empty project
// is "default". ErrTemplatedProject is returned when any of them is templated.
func Projects(appSet *v1alpha1.ApplicationSet) ([]string, error) {
	if appSet == nil {
		return nil, errors.New("error creating ApplicationSets: ApplicationSets is nil in request")
	}
	set := map[string]bool{}
	add := func(p string) error {
		if strings.Contains(p, "{{") {
			return ErrTemplatedProject
		}
		if p == "" {
			p = "default"
		}
		set[p] = true
		return nil
	}
	if err := add(appSet.Spec.Template.Spec.Project); err != nil {
		return nil, err
	}
	kinds := generators.New(generators.Config{})
	for i := range appSet.Spec.Generators {
		entry := &appSet.Spec.Generators[i]
		for _, kind := range generators.Relevant(entry) {
			g, ok := kinds[kind]
			if !ok {
				continue
			}
			if t := g.Template(entry); t != nil && t.Spec.Project != "" {
				if err := add(t.Spec.Project); err != nil {
					return nil, err
				}
			}
		}
	}
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out, nil
}

// APIGenerator renders ApplicationSets for the API server's dry-run endpoint
// (POST /api/v1/applicationsets/generate) without validating projects or destinations and
// without writing anything. Generators run with the API server's settings, always in tokenRef
// strict mode and without the SCM proxy, as upstream.
type APIGenerator struct {
	cfg generators.Config
}

// NewAPIGenerator returns an APIGenerator. cfg.Client must read the API server's namespace
// (cfg.Namespace) and the ApplicationSet namespaces.
func NewAPIGenerator(cfg generators.Config) *APIGenerator {
	cfg.SCM.TokenRefStrictMode = true
	cfg.SCM.ProxyURL, cfg.SCM.NoProxy = "", ""
	return &APIGenerator{cfg: cfg}
}

// maxCapturedLogBytes bounds the generator logs appended to Generate errors.
const maxCapturedLogBytes = 16 << 10

// Generate returns the Applications appSet generates. On failure the error carries the
// generator logs captured during the call: "unable to generate Applications of
// ApplicationSet: <err>\n<logs>".
func (g *APIGenerator) Generate(ctx context.Context, appSet *v1alpha1.ApplicationSet) ([]v1alpha1.Application, error) {
	if appSet == nil {
		return nil, errors.New("error creating ApplicationSets: ApplicationSets is nil in request")
	}
	buf := &boundedBuffer{max: maxCapturedLogBytes}
	log := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg := g.cfg
	cfg.Logger = log
	cfg.SCM.Logger = log
	set := generators.New(cfg)
	apps, _, err := GenerateApplications(ctx, log, appSet, set)
	if err != nil {
		return nil, fmt.Errorf("unable to generate Applications of ApplicationSet: %w\n%s", err, buf.String())
	}
	if apps == nil {
		apps = []v1alpha1.Application{}
	}
	return apps, nil
}

// boundedBuffer keeps the first max bytes written to it (concurrency-safe).
type boundedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
	max int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if room := b.max - b.buf.Len(); room > 0 {
		if len(p) > room {
			b.buf.Write(p[:room])
		} else {
			b.buf.Write(p)
		}
	}
	return len(p), nil
}

func (b *boundedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// Evaluator evaluates generators for the webhook's Matrix matching (webhook.GeneratorEvaluator).
type Evaluator struct {
	set generators.Set
}

// NewEvaluator returns an Evaluator over the generators configured by cfg.
func NewEvaluator(cfg generators.Config) *Evaluator {
	return &Evaluator{set: generators.New(cfg)}
}

// GenerateParams returns the parameter sets gen produces for appSet (post-selector applied).
func (e *Evaluator) GenerateParams(ctx context.Context, appSet *v1alpha1.ApplicationSet, gen *v1alpha1.ApplicationSetGenerator) ([]map[string]any, error) {
	if appSet == nil || gen == nil {
		return nil, generators.ErrEmptyAppSetGenerator
	}
	results, err := e.set.Transform(ctx, *gen, appSet.Spec.Template, appSet, nil)
	if err != nil {
		return nil, err
	}
	var out []map[string]any
	for _, r := range results {
		out = append(out, r.Params...)
	}
	return out, nil
}

// InterpolateGenerator renders the templated fields of gen with params using appSet's
// templating options.
func (e *Evaluator) InterpolateGenerator(appSet *v1alpha1.ApplicationSet, gen *v1alpha1.ApplicationSetGenerator, params map[string]any) (*v1alpha1.ApplicationSetGenerator, error) {
	return template.InterpolateGenerator(gen, params, generators.TemplateOptions(appSet))
}
