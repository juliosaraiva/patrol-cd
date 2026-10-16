package webhook

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
	"github.com/juliosaraiva/patrol-cd/pkg/scm"
)

// process matches one event and enqueues its refreshes. On failure the delivery is
// forgotten so that a redelivery is processed again.
func (h *Handler) process(ctx context.Context, qe *queuedEvent, start time.Time) {
	var (
		apps, appSets int
		err           error
		attrs         []any
	)
	switch {
	case qe.registry != nil:
		ev := qe.registry
		attrs = []any{"event", "package", "deliveryID", ev.DeliveryID, "verified", ev.Verified, "repo", logValue(ev.ociURL()), "revision", logValue(ev.Tag)}
		apps, err = h.processRegistry(ctx, ev)
	case qe.event != nil:
		ev := qe.event
		repo := ""
		if ev.Repository != nil {
			repo = logValue(scm.RedactURLString(ev.Repository.HTMLURL))
		}
		// Payload fields are untrusted (unsigned deliveries are accepted when no secret is
		// configured): they are clipped before logging.
		attrs = []any{"event", string(ev.Type), "action", logValue(ev.Action), "deliveryID", ev.DeliveryID, "verified", ev.Verified, "repo", repo, "revision", logValue(ev.Revision)}
		switch ev.Type {
		case scm.EventPush:
			h.bitbucket.refine(ctx, ev)
			apps, appSets, err = h.processPush(ctx, ev)
		case scm.EventPullRequest:
			appSets, err = h.refreshApplicationSets(ctx, &appSetEvent{pr: newPRInfo(ev)})
		}
	default:
		return
	}
	attrs = append(attrs, "provider", qe.provider, "applications", apps, "applicationSets", appSets, "duration", time.Since(start).String())
	if err != nil {
		h.deliveries.Forget(qe.deliveryKey)
		h.log.Error("webhook: event processing failed", append(attrs, "error", logErr(err))...)
		return
	}
	// Audit record: who sent what and what it triggered; never bodies or secrets.
	h.log.Info("webhook: event processed", attrs...)
}

// processPush refreshes the Applications and ApplicationSets a push affects.
func (h *Handler) processPush(ctx context.Context, ev *scm.Event) (apps, appSets int, err error) {
	var regexps []*regexp.Regexp
	for _, u := range pushWebURLs(ev) {
		re, rerr := WebURLRegex(u)
		if rerr != nil {
			h.log.Info("webhook: repository URL skipped", "error", logErr(rerr))
			continue
		}
		regexps = append(regexps, re)
	}
	if len(regexps) == 0 {
		return 0, 0, nil
	}
	apps, err = h.refreshApplications(ctx, ev, regexps, h.cfg.Settings())
	appSets, serr := h.refreshApplicationSets(ctx, &appSetEvent{push: newPushInfo(ev, regexps)})
	return apps, appSets, errors.Join(err, serr)
}

// refreshApplications enqueues refreshes for the Applications a push affects (REPO-124
// to REPO-127, REPO-129, REPO-130). Per repository URL, refreshes beyond
// webhook.refresh.jitter.threshold are delayed by a random duration below
// webhook.refresh.jitter.
func (h *Handler) refreshApplications(ctx context.Context, ev *scm.Event, regexps []*regexp.Regexp, ws settings.Webhook) (int, error) {
	list, err := h.listApplications(ctx)
	if err != nil {
		return 0, err
	}
	refreshed := map[refreshRequest]bool{}
	for _, re := range regexps {
		affected := 0
		for i := range list {
			app := &list[i]
			hydrate, ok := matchAppPush(app, re, ev)
			if !ok {
				continue
			}
			affected++
			req := refreshRequest{kind: kindApplication, namespace: app.Namespace, name: app.Name, hydrate: hydrate}
			var delay time.Duration
			if ws.RefreshJitter > 0 && affected > ws.RefreshJitterThreshold {
				delay = time.Duration(float64(ws.RefreshJitter) * h.randFloat())
			}
			h.enqueueRefresh(req, delay)
			refreshed[req] = true
		}
	}
	return len(refreshed), nil
}

// appSources returns every source a push can affect: the spec sources (the sync source
// for hydrated Applications, every source of multi-source Applications including
// ref-only sources) and the hydrator's dry source.
func appSources(app *v1alpha1.Application) (sources []v1alpha1.ApplicationSource, dry, sync *v1alpha1.ApplicationSource) {
	sources = append(sources, app.Spec.GetSources()...)
	if hz := app.Spec.SourceHydrator; hz != nil {
		d, s := hz.GetDrySource(), hz.GetSyncSource()
		dry, sync = &d, &s
		sources = append(sources, d)
	}
	return sources, dry, sync
}

// matchAppPush reports whether a push to the repository matched by re affects app, and
// whether the hydrator's dry source is the affected source (then hydration is requested
// too). The first affected source decides, as upstream.
func matchAppPush(app *v1alpha1.Application, re *regexp.Regexp, ev *scm.Event) (hydrate, ok bool) {
	sources, dry, sync := appSources(app)
	annotation := app.Annotations[v1alpha1.AnnotationKeyManifestGeneratePaths]
	for _, src := range sources {
		if !sourceRevisionHasChanged(src.TargetRevision, ev.Revision, ev.TouchedDefaultBranch) || !re.MatchString(src.RepoURL) {
			continue
		}
		isSync := sync != nil && equality.Semantic.DeepEqual(src, *sync)
		paths := refreshPaths(annotation, src.Path, isSync)
		// A truncated (or provider-less) changed-file list means every path may have
		// changed.
		if ev.ChangedFilesTruncated || filesHaveChanged(paths, ev.ChangedFiles) {
			return dry != nil && equality.Semantic.DeepEqual(src, *dry), true
		}
	}
	return false, false
}

// processRegistry refreshes the Applications whose OCI source is the published
// repository at a matching tag (REPO-123). Hydration is requested when the matching
// source is the hydrator's dry source. No jitter is applied, as upstream.
func (h *Handler) processRegistry(ctx context.Context, ev *registryEvent) (int, error) {
	list, err := h.listApplications(ctx)
	if err != nil {
		return 0, err
	}
	want := normalizeOCI(ev.ociURL())
	n := 0
	for i := range list {
		app := &list[i]
		sources, dry, _ := appSources(app)
		for _, src := range sources {
			if normalizeOCI(src.RepoURL) != want || !compareRevisions(ev.Tag, src.TargetRevision) {
				continue
			}
			hydrate := dry != nil && equality.Semantic.DeepEqual(src, *dry)
			h.enqueueRefresh(refreshRequest{kind: kindApplication, namespace: app.Namespace, name: app.Name, hydrate: hydrate}, 0)
			n++
			break
		}
	}
	return n, nil
}

// refreshApplicationSets enqueues refreshes for the ApplicationSets an event affects.
func (h *Handler) refreshApplicationSets(ctx context.Context, ev *appSetEvent) (int, error) {
	if ev == nil || (ev.push == nil && ev.pr == nil) {
		return 0, nil
	}
	list, err := h.listApplicationSets(ctx)
	if err != nil {
		return 0, err
	}
	apiCache := map[client.ObjectKey]string{}
	n := 0
	for i := range list {
		as := &list[i]
		ectx, cancel := context.WithTimeout(ctx, h.cfg.APITimeout)
		m := &appSetMatcher{
			ctx:    ectx,
			appSet: as,
			ev:     ev,
			eval:   h.cfg.GeneratorEvaluator,
			log:    h.log,
			providerAPI: func(name string) string {
				return h.scmProviderAPI(ectx, client.ObjectKey{Namespace: as.Namespace, Name: name}, apiCache)
			},
		}
		matched := m.matches()
		cancel()
		if matched {
			h.enqueueRefresh(refreshRequest{kind: kindApplicationSet, namespace: as.Namespace, name: as.Name}, 0)
			n++
		}
	}
	return n, nil
}

// scmProviderAPI returns the API URL of an SCMProvider referenced by a generator's
// providerRef ("" when it cannot be read).
func (h *Handler) scmProviderAPI(ctx context.Context, key client.ObjectKey, cache map[client.ObjectKey]string) string {
	if v, ok := cache[key]; ok {
		return v
	}
	var sp v1alpha1.SCMProvider
	v := ""
	if err := h.cfg.Reader.Get(ctx, key, &sp); err == nil {
		v = sp.Spec.APIURL
	} else {
		h.log.Info("webhook: SCMProvider referenced by a generator unavailable", "scmProvider", key.String(), "error", logErr(err))
	}
	cache[key] = v
	return v
}

// listApplications lists the Applications of the allowed namespaces.
func (h *Handler) listApplications(ctx context.Context) ([]v1alpha1.Application, error) {
	var list v1alpha1.ApplicationList
	if err := h.list(ctx, &list, h.appNS); err != nil {
		return nil, fmt.Errorf("list applications: %w", err)
	}
	out := list.Items[:0]
	for _, a := range list.Items {
		if h.appNS.allowed(a.Namespace) {
			out = append(out, a)
		}
	}
	return out, nil
}

// listApplicationSets lists the ApplicationSets of the allowed namespaces.
func (h *Handler) listApplicationSets(ctx context.Context) ([]v1alpha1.ApplicationSet, error) {
	var list v1alpha1.ApplicationSetList
	if err := h.list(ctx, &list, h.appSetNS); err != nil {
		return nil, fmt.Errorf("list applicationsets: %w", err)
	}
	out := list.Items[:0]
	for _, a := range list.Items {
		if h.appSetNS.allowed(a.Namespace) {
			out = append(out, a)
		}
	}
	return out, nil
}

func (h *Handler) list(ctx context.Context, list client.ObjectList, ns *namespaceMatcher) error {
	cctx, cancel := context.WithTimeout(ctx, h.cfg.APITimeout)
	defer cancel()
	var opts []client.ListOption
	if !ns.clusterWide() {
		opts = append(opts, client.InNamespace(h.cfg.Namespace))
	}
	return h.cfg.Reader.List(cctx, list, opts...)
}
