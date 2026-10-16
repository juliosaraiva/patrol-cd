package tracking

import (
	"errors"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// ErrWrongResourceTrackingFormat is returned by [ParseTrackingID] for values that are not
// "<application-name>:<group>/<kind>:<namespace>/<name>". The message is the upstream one.
var ErrWrongResourceTrackingFormat = errors.New("wrong resource tracking format, should be <application-name>:<group>/<kind>:<namespace>/<name>")

// AppInstanceValue is the content of a tracking id: the instance name of the Application
// and the identity of the resource it was written for.
type AppInstanceValue struct {
	// ApplicationName is the instance name of the Application (see [InstanceName]).
	ApplicationName string
	// Namespace is the resource namespace, or the Application's destination namespace for
	// cluster-scoped resources and resources rendered without a namespace.
	Namespace string
	// Group is the API group of the resource ("" for the core group).
	Group string
	// Kind is the resource kind.
	Kind string
	// Name is the resource name.
	Name string
}

// String returns the tracking id of v (see [BuildTrackingID]).
func (v AppInstanceValue) String() string { return BuildTrackingID(v) }

// References reports whether v identifies obj: group, kind and name are equal, and the
// namespace is equal or obj is cluster-scoped (has no namespace). Cluster-scoped objects
// carry the destination namespace in their tracking id but are unique by group, kind and
// name. The application name is not compared.
func (v AppInstanceValue) References(obj *unstructured.Unstructured) bool {
	if obj == nil {
		return false
	}
	gvk := obj.GroupVersionKind()
	return (obj.GetNamespace() == v.Namespace || obj.GetNamespace() == "") &&
		obj.GetName() == v.Name &&
		gvk.Group == v.Group &&
		gvk.Kind == v.Kind
}

// BuildTrackingID formats v as "<application-name>:<group>/<kind>:<namespace>/<name>".
//
// [ParseTrackingID] inverts it whenever the application name contains no ':', group and kind
// contain neither ':' nor '/', and namespace and name contain no '/' — which holds for every
// valid Kubernetes object and Application.
func BuildTrackingID(v AppInstanceValue) string {
	var b strings.Builder
	b.Grow(len(v.ApplicationName) + len(v.Group) + len(v.Kind) + len(v.Namespace) + len(v.Name) + 4)
	b.WriteString(v.ApplicationName)
	b.WriteByte(':')
	b.WriteString(v.Group)
	b.WriteByte('/')
	b.WriteString(v.Kind)
	b.WriteByte(':')
	b.WriteString(v.Namespace)
	b.WriteByte('/')
	b.WriteString(v.Name)
	return b.String()
}

// ParseTrackingID parses "<application-name>:<group>/<kind>:<namespace>/<name>".
//
// As upstream, the value is split on the first two ':' only, so the name may contain ':'
// (e.g. "system:aggregate-to-admin"), while "<group>/<kind>" and "<namespace>/<name>" must
// each contain exactly one '/'. Any other shape returns [ErrWrongResourceTrackingFormat].
// For every value it accepts, BuildTrackingID(*ParseTrackingID(s)) == s.
func ParseTrackingID(s string) (*AppInstanceValue, error) {
	app, rest, ok := strings.Cut(s, ":")
	if !ok {
		return nil, ErrWrongResourceTrackingFormat
	}
	groupKind, nsName, ok := strings.Cut(rest, ":")
	if !ok {
		return nil, ErrWrongResourceTrackingFormat
	}
	group, kind, ok := cutOnce(groupKind, "/")
	if !ok {
		return nil, ErrWrongResourceTrackingFormat
	}
	ns, name, ok := cutOnce(nsName, "/")
	if !ok {
		return nil, ErrWrongResourceTrackingFormat
	}
	return &AppInstanceValue{ApplicationName: app, Namespace: ns, Group: group, Kind: kind, Name: name}, nil
}

// cutOnce splits s around sep and reports whether sep occurs exactly once.
func cutOnce(s, sep string) (before, after string, ok bool) {
	before, after, ok = strings.Cut(s, sep)
	if !ok || strings.Contains(after, sep) {
		return "", "", false
	}
	return before, after, true
}

// NewAppInstanceValue returns the tracking id content for obj managed by the Application
// with the given instance name. defaultNamespace (the Application's destination namespace)
// is used when obj has no namespace.
func NewAppInstanceValue(obj *unstructured.Unstructured, instanceName, defaultNamespace string) AppInstanceValue {
	ns := obj.GetNamespace()
	if ns == "" {
		ns = defaultNamespace
	}
	gvk := obj.GroupVersionKind()
	return AppInstanceValue{
		ApplicationName: instanceName,
		Namespace:       ns,
		Group:           gvk.Group,
		Kind:            gvk.Kind,
		Name:            obj.GetName(),
	}
}
