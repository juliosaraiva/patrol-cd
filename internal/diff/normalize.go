package diff

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
)

const rbacGroup = "rbac.authorization.k8s.io"

// Normalize applies the built-in normalizations and o.Normalizer to un in place:
//   - metadata.creationTimestamp is removed (exported manifests often carry null);
//   - Secrets: stringData is merged into data (base64), data values are canonicalized;
//   - Roles/ClusterRoles: an empty rules list becomes null, and with IgnoreAggregatedRoles the
//     rules of aggregated roles are cleared;
//   - Endpoints: subsets, addresses and ports are sorted and port protocols defaulted.
//
// Normalizer errors are logged and do not abort normalization, matching the baseline.
func Normalize(ctx context.Context, un *unstructured.Unstructured, o Options) {
	o = o.withDefaults()
	normalizeObject(ctx, un, o, false)
}

// normalizeObject implements Normalize. With skipFull, the user-configured normalizer is not
// applied (server-side diff applies it after the dry run instead).
func normalizeObject(ctx context.Context, un *unstructured.Unstructured, o Options, skipFull bool) {
	if un == nil || un.Object == nil {
		return
	}
	unstructured.RemoveNestedField(un.Object, "metadata", "creationTimestamp")

	gvk := un.GroupVersionKind()
	switch {
	case gvk.Group == "" && gvk.Kind == "Secret":
		normalizeSecret(un, o.Log)
	case gvk.Group == rbacGroup && (gvk.Kind == "ClusterRole" || gvk.Kind == "Role"):
		normalizeRole(un, o.IgnoreAggregatedRoles, o.Log)
	case gvk.Group == "" && gvk.Kind == "Endpoints":
		normalizeEndpoints(un, o.Log)
	}

	if skipFull || o.Normalizer == nil {
		return
	}
	if err := o.Normalizer.Normalize(ctx, un); err != nil {
		o.Log.Error("failed to normalize resource",
			"kind", gvk.Kind, "group", gvk.Group, "namespace", un.GetNamespace(), "name", un.GetName(),
			"error", sanitizeError(un, err))
	}
}

// NormalizeSecret merges stringData into data (base64-encoding string values, copying other
// values unchanged so that invalid manifests still fail to apply) and canonicalizes data
// values (null becomes the empty string). Objects that are not core/v1 Secrets are left
// untouched.
func NormalizeSecret(un *unstructured.Unstructured) {
	normalizeSecret(un, nil)
}

func normalizeSecret(un *unstructured.Unstructured, log *slog.Logger) {
	if un == nil || un.Object == nil {
		return
	}
	gvk := un.GroupVersionKind()
	if gvk.Group != "" || gvk.Kind != "Secret" {
		return
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if stringData, found, err := unstructured.NestedMap(un.Object, "stringData"); found && err == nil {
		data, found, _ := unstructured.NestedMap(un.Object, "data")
		if !found || data == nil {
			data = map[string]any{}
		}
		for k, v := range stringData {
			if s, ok := v.(string); ok {
				data[k] = base64.StdEncoding.EncodeToString([]byte(s))
			} else {
				data[k] = v
			}
		}
		if err := unstructured.SetNestedField(un.Object, data, "data"); err == nil {
			delete(un.Object, "stringData")
		}
	}

	var secret corev1.Secret
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(un.Object, &secret); err != nil {
		// Never include the conversion error text: it can quote Secret values.
		log.Debug("secret is not a valid core/v1 Secret; data left as-is", "namespace", un.GetNamespace(), "name", un.GetName())
		return
	}
	if secret.Data == nil {
		return
	}
	for k, v := range secret.Data {
		if len(v) == 0 {
			secret.Data[k] = []byte{}
		}
	}
	converted, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&secret)
	if err != nil {
		log.Debug("secret could not be re-encoded; data left as-is", "namespace", un.GetNamespace(), "name", un.GetName())
		return
	}
	if err := unstructured.SetNestedField(un.Object, converted["data"], "data"); err != nil {
		log.Debug("failed to set secret data", "namespace", un.GetNamespace(), "name", un.GetName())
	}
}

// normalizeRole turns an empty rules list into null and, when ignoreAggregated is set, clears
// the rules of roles with an aggregationRule (the aggregation controller owns them).
func normalizeRole(un *unstructured.Unstructured, ignoreAggregated bool, log *slog.Logger) {
	if ignoreAggregated {
		if rule, ok := un.Object["aggregationRule"]; ok {
			if _, isMap := rule.(map[string]any); isMap {
				un.Object["rules"] = nil
			} else {
				log.Info("malformed aggregationRule; rules not ignored", "kind", un.GetKind(), "name", un.GetName())
			}
		}
	}
	rules, ok := un.Object["rules"].([]any)
	if ok && rules != nil && len(rules) == 0 {
		un.Object["rules"] = nil
	}
}

// normalizeEndpoints defaults port protocols to TCP and sorts subsets, addresses and ports
// into a canonical order so that ordering produced by the endpoints controller is not drift.
func normalizeEndpoints(un *unstructured.Unstructured, log *slog.Logger) {
	var ep corev1.Endpoints //nolint:staticcheck // Endpoints is deprecated but still a valid managed resource.
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(un.Object, &ep); err != nil {
		log.Debug("could not convert Endpoints; left as-is", "name", un.GetName(), "error", err)
		return
	}
	for s := range ep.Subsets {
		subset := &ep.Subsets[s]
		for p := range subset.Ports {
			if subset.Ports[p].Protocol == "" {
				subset.Ports[p].Protocol = corev1.ProtocolTCP
			}
		}
		sortAddresses(subset.Addresses)
		sortAddresses(subset.NotReadyAddresses)
		sortByDigest(subset.Ports)
	}
	sortByDigest(ep.Subsets)
	converted, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&ep)
	if err != nil {
		log.Debug("could not re-encode Endpoints; left as-is", "name", un.GetName(), "error", err)
		return
	}
	un.Object = converted
}

func sortAddresses(addrs []corev1.EndpointAddress) {
	slices.SortStableFunc(addrs, func(a, b corev1.EndpointAddress) int {
		if c := strings.Compare(a.IP, b.IP); c != 0 {
			return c
		}
		switch {
		case a.TargetRef == nil && b.TargetRef == nil:
			return 0
		case a.TargetRef == nil:
			return -1
		case b.TargetRef == nil:
			return 1
		}
		return strings.Compare(string(a.TargetRef.UID), string(b.TargetRef.UID))
	})
}

// sortByDigest orders items by the SHA-256 of their JSON encoding: an arbitrary but stable
// total order that only depends on content.
func sortByDigest[T any](items []T) {
	type keyed struct {
		key  []byte
		item T
	}
	ks := make([]keyed, len(items))
	for i, it := range items {
		data, err := json.Marshal(it)
		if err != nil {
			data = fmt.Appendf(nil, "%#v", it)
		}
		sum := sha256.Sum256(data)
		ks[i] = keyed{key: sum[:], item: it}
	}
	slices.SortStableFunc(ks, func(a, b keyed) int { return bytes.Compare(a.key, b.key) })
	for i := range ks {
		items[i] = ks[i].item
	}
}

// remarshal re-encodes obj through its typed Go struct when the GroupVersionKind is a
// built-in type and the object has no unknown fields. This applies the custom JSON encoders
// of Kubernetes types (quantities, int-or-string, omitempty) so that equivalent manifests
// serialize identically. Fields that the typed round trip adds (e.g. creationTimestamp:
// null) are dropped again. Objects with unknown fields or unregistered kinds are returned
// after a plain JSON round trip, which still strips Go type information.
func remarshal(obj *unstructured.Unstructured, log *slog.Logger) (*unstructured.Unstructured, error) {
	data, err := json.Marshal(obj.Object)
	if err != nil {
		return nil, fmt.Errorf("encode %s: %w", describe(obj), err)
	}
	m, err := decodeObject(data)
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", describe(obj), err)
	}
	plain := &unstructured.Unstructured{Object: m}
	gvk := plain.GroupVersionKind()
	item, err := scheme.Scheme.New(gvk)
	if err != nil {
		return plain, nil //nolint:nilerr // not a built-in kind: nothing to re-encode
	}
	typed := reflect.New(reflect.TypeOf(item).Elem()).Interface()
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(typed); err != nil {
		log.Debug("object does not match its built-in type; compared as-is", "gvk", gvk.String(), "name", plain.GetName(), "error", sanitizeError(plain, err))
		return plain, nil //nolint:nilerr // unknown fields: compare the manifest as written (baseline behavior)
	}
	converted, err := runtime.DefaultUnstructuredConverter.ToUnstructured(typed)
	if err != nil {
		log.Debug("object could not be converted back to unstructured; compared as-is", "gvk", gvk.String(), "name", plain.GetName())
		return plain, nil //nolint:nilerr // see above
	}
	return &unstructured.Unstructured{Object: trimToShape(plain.Object, converted)}, nil
}

// sanitizeError returns an error message that is safe to log for obj: messages about Secrets
// are reduced to their error type because they may quote secret values. Other messages are
// truncated to bound log volume.
func sanitizeError(obj *unstructured.Unstructured, err error) string {
	if err == nil {
		return ""
	}
	if obj != nil && obj.GroupVersionKind().Group == "" && obj.GetKind() == "Secret" {
		return fmt.Sprintf("%T (message redacted for Secret)", err)
	}
	const maxLen = 512
	msg := err.Error()
	if len(msg) > maxLen {
		msg = msg[:maxLen] + "...(truncated)"
	}
	return msg
}
