package treeapi

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/juliosaraiva/patrol-cd/internal/compare"
	"github.com/juliosaraiva/patrol-cd/internal/diff"
)

// HandlerOptions configure the server side.
type HandlerOptions struct {
	// Token is the shared internal token (Secret patrol-internal-token, ADR-0006). Required.
	Token string
	// MaxRequestBytes caps request bodies (DefaultMaxRequestBytes).
	MaxRequestBytes int64
	// MaxDiffResources caps the resources of a server-side diff (DefaultMaxDiffResources).
	MaxDiffResources int
	// Logger receives server errors (nil discards).
	Logger *slog.Logger
	// Registerer receives the request metrics (nil: not registered).
	Registerer prometheus.Registerer
}

// NewHandler returns the tree API handler over provider.
func NewHandler(provider StateProvider, opts HandlerOptions) (http.Handler, error) {
	if provider == nil {
		return nil, errors.New("treeapi: state provider is required")
	}
	if opts.Token == "" {
		return nil, errors.New("treeapi: an internal token is required")
	}
	if opts.MaxRequestBytes <= 0 {
		opts.MaxRequestBytes = DefaultMaxRequestBytes
	}
	if opts.MaxDiffResources <= 0 {
		opts.MaxDiffResources = DefaultMaxDiffResources
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	m, err := newServerMetrics(opts.Registerer)
	if err != nil {
		return nil, err
	}
	h := &handler{p: provider, opts: opts, tokenSum: sha256.Sum256([]byte(opts.Token)), metrics: m}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+PathHealthz, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.Handle("GET "+PathTree, h.route("tree", h.tree))
	mux.Handle("GET "+PathManagedResources, h.route("managed-resources", h.managedResources))
	mux.Handle("POST "+PathServerSideDiff, h.route("server-side-diff", h.serverSideDiff))
	mux.Handle("GET "+PathResource, h.route("resource", h.resource))
	return mux, nil
}

type handler struct {
	p        StateProvider
	opts     HandlerOptions
	tokenSum [sha256.Size]byte
	metrics  *serverMetrics
}

// statusRecorder captures the status for metrics.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (h *handler) route(name string, fn func(http.ResponseWriter, *http.Request) error) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		defer func() {
			h.metrics.observe(name, rec.status, time.Since(start))
		}()
		if !h.authorized(r) {
			rec.Header().Set("WWW-Authenticate", `Bearer realm="patrol-controller"`)
			writeProblem(rec, http.StatusUnauthorized, CodeUnauthenticated, "missing or invalid internal token", nil)
			return
		}
		if err := fn(rec, r); err != nil {
			status, code, shard := classify(err)
			if status >= http.StatusInternalServerError && status != http.StatusServiceUnavailable {
				h.opts.Logger.Error("controller tree API request failed", slog.String("route", name),
					slog.String("application", r.URL.Query().Get("namespace")+"/"+r.URL.Query().Get("name")), slog.Any("error", err))
			}
			writeProblem(rec, status, code, err.Error(), shard)
		}
	})
}

func (h *handler) authorized(r *http.Request) bool {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || token == "" {
		return false
	}
	sum := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(sum[:], h.tokenSum[:]) == 1
}

// appKey validates the namespace and name of the Application.
func appKey(namespace, name string) (AppKey, error) {
	if namespace == "" || name == "" {
		return AppKey{}, invalidf("namespace and name are required")
	}
	if errs := validation.IsDNS1123Label(namespace); len(errs) > 0 {
		return AppKey{}, invalidf("invalid namespace %q: %s", namespace, strings.Join(errs, "; "))
	}
	if errs := validation.IsDNS1123Subdomain(name); len(errs) > 0 {
		return AppKey{}, invalidf("invalid name %q: %s", name, strings.Join(errs, "; "))
	}
	return AppKey{Namespace: namespace, Name: name}, nil
}

// maxFieldLen bounds resource identity fields in queries.
const maxFieldLen = 253

func field(q url.Values, key string) (string, error) {
	v := q.Get(key)
	if len(v) > maxFieldLen || !utf8.ValidString(v) || strings.ContainsFunc(v, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return "", invalidf("invalid %s", key)
	}
	return v, nil
}

func fields(q url.Values, keys ...string) ([]string, error) {
	out := make([]string, len(keys))
	for i, k := range keys {
		v, err := field(q, k)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

func queryApp(r *http.Request) (AppKey, url.Values, error) {
	q := r.URL.Query()
	key, err := appKey(q.Get("namespace"), q.Get("name"))
	return key, q, err
}

func (h *handler) tree(w http.ResponseWriter, r *http.Request) error {
	key, _, err := queryApp(r)
	if err != nil {
		return err
	}
	tree, err := h.p.Tree(r.Context(), key)
	if err != nil {
		return err
	}
	if tree == nil {
		tree = &compare.ApplicationTree{}
	}
	body, err := json.Marshal(tree)
	if err != nil {
		return fmt.Errorf("encode resource tree: %w", err)
	}
	sum := sha256.Sum256(body)
	etag := `"` + hex.EncodeToString(sum[:16]) + `"`
	w.Header().Set("ETag", etag)
	if inm := r.Header.Get("If-None-Match"); inm != "" && inm == etag {
		w.WriteHeader(http.StatusNotModified)
		return nil
	}
	writeRawJSON(w, body)
	return nil
}

func (h *handler) managedResources(w http.ResponseWriter, r *http.Request) error {
	key, q, err := queryApp(r)
	if err != nil {
		return err
	}
	f, err := fields(q, "group", "kind", "resourceNamespace", "resourceName")
	if err != nil {
		return err
	}
	filter := ManagedResourcesQuery{Group: f[0], Kind: f[1], ResourceNamespace: f[2], ResourceName: f[3]}
	managed, err := h.p.ManagedResources(r.Context(), key)
	if err != nil {
		return err
	}
	hide := hideSet(h.p.SensitiveAnnotations())
	out := ManagedResourcesResponse{Items: []ResourceDiff{}}
	for i := range managed {
		mr := &managed[i]
		if mr.Hook || !matches(filter, mr.Key) {
			continue
		}
		item, err := managedItem(mr, hide)
		if err != nil {
			return fmt.Errorf("managed resource %s/%s %s/%s: %w", mr.Key.Group, mr.Key.Kind, mr.Key.Namespace, mr.Key.Name, err)
		}
		out.Items = append(out.Items, item)
	}
	return writeJSON(w, out)
}

func matches(q ManagedResourcesQuery, k compare.ResourceKey) bool {
	return (q.ResourceName == "" || q.ResourceName == k.Name) &&
		(q.ResourceNamespace == "" || q.ResourceNamespace == k.Namespace) &&
		(q.Group == "" || q.Group == k.Group) &&
		(q.Kind == "" || q.Kind == k.Kind)
}

// managedItem converts a managed resource, masking core Secrets (both object sides and the
// diff pair, each as a pair so differences stay visible).
func managedItem(mr *compare.ManagedResource, hide map[string]bool) (ResourceDiff, error) {
	target, live, res := mr.Target, mr.Live, mr.Diff
	if (mr.Key.Group == "" && mr.Key.Kind == "Secret") || isCoreSecret(target) || isCoreSecret(live) {
		var err error
		if target, live, err = diff.HideSecretData(target, live, hide); err != nil {
			return ResourceDiff{}, fmt.Errorf("error hiding secret data: %w", err)
		}
		masked, err := diff.MaskSecretResult(&res, hide)
		if err != nil {
			return ResourceDiff{}, fmt.Errorf("error hiding secret data in diff result: %w", err)
		}
		if masked != nil {
			res = *masked
		}
	}
	targetState, err := objectJSON(target)
	if err != nil {
		return ResourceDiff{}, err
	}
	liveState, err := objectJSON(live)
	if err != nil {
		return ResourceDiff{}, err
	}
	return ResourceDiff{
		Group:               mr.Key.Group,
		Kind:                mr.Key.Kind,
		Namespace:           mr.Key.Namespace,
		Name:                mr.Key.Name,
		TargetState:         targetState,
		LiveState:           liveState,
		Hook:                mr.Hook,
		NormalizedLiveState: string(res.NormalizedLive),
		PredictedLiveState:  string(res.PredictedLive),
		ResourceVersion:     mr.ResourceVersion,
		Modified:            res.Modified,
	}, nil
}

// objectJSON is the JSON of obj, "null" for nil (upstream's literal for missing sides).
func objectJSON(obj *unstructured.Unstructured) (string, error) {
	if obj == nil {
		return "null", nil
	}
	b, err := json.Marshal(obj.Object)
	if err != nil {
		return "", fmt.Errorf("encode object: %w", err)
	}
	return string(b), nil
}

func isCoreSecret(obj *unstructured.Unstructured) bool {
	if obj == nil {
		return false
	}
	gvk := obj.GroupVersionKind()
	return gvk.Group == "" && gvk.Kind == "Secret"
}

func hideSet(keys []string) map[string]bool {
	out := make(map[string]bool, len(keys))
	for _, k := range keys {
		if k = strings.TrimSpace(k); k != "" {
			out[k] = true
		}
	}
	return out
}

// parseManifest decodes a JSON object ("" and "null" are nil).
func parseManifest(s string) (*unstructured.Unstructured, error) {
	if s == "" || s == "null" {
		return nil, nil
	}
	var obj unstructured.Unstructured
	if err := json.Unmarshal([]byte(s), &obj.Object); err != nil {
		return nil, err
	}
	if obj.Object == nil {
		return nil, nil
	}
	return &obj, nil
}

func (h *handler) serverSideDiff(w http.ResponseWriter, r *http.Request) error {
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		return invalidf("content type must be application/json")
	}
	var req ServerSideDiffRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, h.opts.MaxRequestBytes))
	if err := dec.Decode(&req); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return fmt.Errorf("request body exceeds %d bytes: %w", mbe.Limit, ErrTooLarge)
		}
		return invalidf("malformed request body: %v", err)
	}
	if dec.More() {
		return invalidf("malformed request body: trailing data")
	}
	key, err := appKey(req.Namespace, req.Name)
	if err != nil {
		return err
	}
	if n := max(len(req.LiveResources), len(req.TargetManifests)); n > h.opts.MaxDiffResources {
		return fmt.Errorf("%d resources exceed the limit of %d per request: %w", n, h.opts.MaxDiffResources, ErrTooLarge)
	}
	if len(req.LiveResources) != len(req.TargetManifests) {
		return invalidf("liveResources and targetManifests must be index-aligned: got %d and %d", len(req.LiveResources), len(req.TargetManifests))
	}

	live := make([]*unstructured.Unstructured, len(req.LiveResources))
	var managed map[compare.ResourceKey]bool
	for i := range req.LiveResources {
		lr := &req.LiveResources[i]
		obj, err := parseManifest(lr.LiveState)
		if err != nil {
			return invalidf("error unmarshaling live state for %s/%s: %v", lr.Kind, lr.Name, err)
		}
		if obj == nil {
			continue
		}
		gvk := obj.GroupVersionKind()
		switch {
		case obj.GetName() != lr.Name:
			return invalidf("name mismatch: expected %s, got %s for live resource %s/%s/%s", lr.Name, obj.GetName(), lr.Group, lr.Kind, lr.Name)
		case obj.GetNamespace() != lr.Namespace:
			return invalidf("namespace mismatch: expected %s, got %s for live resource %s/%s/%s", lr.Namespace, obj.GetNamespace(), lr.Group, lr.Kind, lr.Name)
		case gvk.Group != lr.Group:
			return invalidf("group mismatch: expected %s, got %s for live resource %s/%s/%s", lr.Group, gvk.Group, lr.Group, lr.Kind, lr.Name)
		case gvk.Kind != lr.Kind:
			return invalidf("kind mismatch: expected %s, got %s for live resource %s/%s/%s", lr.Kind, gvk.Kind, lr.Group, lr.Kind, lr.Name)
		}
		if managed == nil {
			list, err := h.p.ManagedResources(r.Context(), key)
			if err != nil {
				return err
			}
			managed = make(map[compare.ResourceKey]bool, len(list))
			for _, mr := range list {
				managed[mr.Key] = true
			}
		}
		if !managed[compare.ResourceKey{Group: lr.Group, Kind: lr.Kind, Namespace: lr.Namespace, Name: lr.Name}] {
			return deniedf("%s/%s %s not found as part of application %s", lr.Group, lr.Kind, lr.Name, key.Name)
		}
		live[i] = obj
	}
	targets := make([]*unstructured.Unstructured, len(req.TargetManifests))
	for i, m := range req.TargetManifests {
		obj, err := parseManifest(m)
		if err != nil {
			return invalidf("error unmarshaling target manifest %d: %v", i, err)
		}
		targets[i] = obj
	}

	results, err := h.p.ServerSideDiff(r.Context(), key, live, targets)
	if err != nil {
		return err
	}
	if len(results) != len(targets) {
		return fmt.Errorf("server-side diff returned %d results for %d resources", len(results), len(targets))
	}
	hide := hideSet(h.p.SensitiveAnnotations())
	resp := ServerSideDiffResponse{Items: make([]ResourceDiff, 0, len(results))}
	for i, res := range results {
		resp.Modified = resp.Modified || res.Modified
		item := ResourceDiff{Modified: res.Modified, TargetState: string(res.PredictedLive), LiveState: string(res.NormalizedLive)}
		lr := req.LiveResources[i]
		if live[i] != nil || lr.Kind != "" || targets[i] == nil {
			item.Group, item.Kind, item.Namespace, item.Name = lr.Group, lr.Kind, lr.Namespace, lr.Name
			item.Hook, item.ResourceVersion = lr.Hook, lr.ResourceVersion
		} else {
			gvk := targets[i].GroupVersionKind()
			item.Group, item.Kind, item.Namespace, item.Name = gvk.Group, gvk.Kind, targets[i].GetNamespace(), targets[i].GetName()
		}
		// Masking is decided from the objects parsed here, never from caller metadata.
		if isCoreSecret(targets[i]) || isCoreSecret(live[i]) {
			masked, err := maskPair(res, hide)
			if err != nil {
				return err
			}
			item.TargetState, item.LiveState = string(masked.PredictedLive), string(masked.NormalizedLive)
		}
		resp.Items = append(resp.Items, item)
	}
	return writeJSON(w, resp)
}

// maskPair masks the data (and sensitive annotations) of both sides of a Secret's diff result
// as a pair, whatever kind the sides decode to.
func maskPair(res diff.Result, hide map[string]bool) (diff.Result, error) {
	predicted, err := parseManifest(string(res.PredictedLive))
	if err != nil {
		return diff.Result{}, fmt.Errorf("error unmarshaling predicted live for secret masking: %w", err)
	}
	normalized, err := parseManifest(string(res.NormalizedLive))
	if err != nil {
		return diff.Result{}, fmt.Errorf("error unmarshaling normalized live for secret masking: %w", err)
	}
	predicted, normalized, err = diff.HideSecretData(predicted, normalized, hide)
	if err != nil {
		return diff.Result{}, fmt.Errorf("error masking secret data: %w", err)
	}
	out := diff.Result{Modified: res.Modified, PredictedLive: res.PredictedLive, NormalizedLive: res.NormalizedLive}
	if predicted != nil {
		if out.PredictedLive, err = json.Marshal(predicted.Object); err != nil {
			return diff.Result{}, fmt.Errorf("error marshaling masked target state: %w", err)
		}
	}
	if normalized != nil {
		if out.NormalizedLive, err = json.Marshal(normalized.Object); err != nil {
			return diff.Result{}, fmt.Errorf("error marshaling masked live state: %w", err)
		}
	}
	return out, nil
}

func (h *handler) resource(w http.ResponseWriter, r *http.Request) error {
	key, q, err := queryApp(r)
	if err != nil {
		return err
	}
	f, err := fields(q, "group", "version", "kind", "resourceNamespace", "resourceName")
	if err != nil {
		return err
	}
	rq := ResourceQuery{Group: f[0], Version: f[1], Kind: f[2], ResourceNamespace: f[3], ResourceName: f[4]}
	if rq.Kind == "" || rq.ResourceName == "" {
		return invalidf("kind and resourceName are required")
	}
	tree, err := h.p.Tree(r.Context(), key)
	if err != nil {
		return err
	}
	node := tree.FindNode(rq.Group, rq.Kind, rq.ResourceNamespace, rq.ResourceName)
	if node == nil || node.UID == "" {
		return invalidf("%s %s %s not found as part of application %s", rq.Kind, rq.Group, rq.ResourceName, key.Name)
	}
	ref := node.ResourceRef
	if rq.Version != "" {
		ref.Version = rq.Version
	}
	obj, err := h.p.LiveResource(r.Context(), key, ref)
	if err != nil {
		return err
	}
	if obj == nil {
		return fmt.Errorf("%s %s %s: %w", rq.Kind, rq.Group, rq.ResourceName, ErrNotFound)
	}
	if isCoreSecret(obj) || (ref.Group == "" && ref.Kind == "Secret") {
		_, masked, err := diff.HideSecretData(nil, obj, hideSet(h.p.SensitiveAnnotations()))
		if err != nil {
			return fmt.Errorf("error replacing secret values: %w", err)
		}
		obj = masked
	}
	manifest, err := objectJSON(obj)
	if err != nil {
		return err
	}
	return writeJSON(w, ResourceResponse{Manifest: manifest})
}

func writeJSON(w http.ResponseWriter, v any) error {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(v); err != nil {
		return fmt.Errorf("encode response: %w", err)
	}
	writeRawJSON(w, buf.Bytes())
	return nil
}

func writeRawJSON(w http.ResponseWriter, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

type problem struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail,omitempty"`
	Code   string `json:"code"`
	Shard  *int   `json:"shard,omitempty"`
}

// writeProblem writes an RFC 9457 problem details response; misdirected responses carry the
// owning shard as the "shard" extension member and the Patrol-Shard header.
func writeProblem(w http.ResponseWriter, status int, code, detail string, shard *int) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if s := shardHeader(shard); s != "" {
		w.Header().Set(HeaderShard, s)
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(problem{
		Type:   "about:blank",
		Title:  http.StatusText(status),
		Status: status,
		Detail: detail,
		Code:   code,
		Shard:  shard,
	})
}
