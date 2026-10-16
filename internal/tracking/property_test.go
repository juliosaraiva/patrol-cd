package tracking

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"pgregory.net/rapid"

	"github.com/juliosaraiva/patrol-cd/internal/diff"
)

// Generators for valid Kubernetes and Application identities.
var (
	genDNSLabel   = rapid.StringMatching(`[a-z0-9]([a-z0-9-]{0,40}[a-z0-9])?`)
	genGroup      = rapid.OneOf(rapid.Just(""), rapid.StringMatching(`[a-z]{1,8}(\.[a-z]{1,8}){0,2}`))
	genVersion    = rapid.StringMatching(`v[1-9]((alpha|beta)[1-9])?`)
	genKind       = rapid.StringMatching(`[A-Z][a-zA-Z0-9]{0,12}`)
	genObjName    = rapid.StringMatching(`[a-z0-9]([a-z0-9.:-]{0,40}[a-z0-9])?`)
	genMethod     = rapid.SampledFrom(allMethods)
	genInstallID  = rapid.OneOf(rapid.Just(""), rapid.StringMatching(`[a-z0-9-]{1,12}`))
	genSeparators = rapid.StringOf(rapid.SampledFrom([]rune{'a', 'Z', '9', ':', '/', '_', '-', '.'}))
)

type genObject struct {
	obj       *unstructured.Unstructured
	defaultNS string
}

func drawObject(t *rapid.T) genObject {
	group := genGroup.Draw(t, "group")
	apiVersion := genVersion.Draw(t, "version")
	if group != "" {
		apiVersion = group + "/" + apiVersion
	}
	ns := ""
	if rapid.Bool().Draw(t, "namespaced") {
		ns = genDNSLabel.Draw(t, "namespace")
	}
	return genObject{
		obj:       newObj(apiVersion, genKind.Draw(t, "kind"), ns, genObjName.Draw(t, "name")),
		defaultNS: rapid.OneOf(rapid.Just(""), genDNSLabel).Draw(t, "defaultNS"),
	}
}

func drawApp(t *rapid.T, controlPlane, label string) App {
	ns := rapid.OneOf(rapid.Just(""), rapid.Just(controlPlane), genDNSLabel).Draw(t, label+"Namespace")
	return App{Name: genDNSLabel.Draw(t, label+"Name"), Namespace: ns}
}

// TestPropertyTrackingIDRoundTrip: ParseTrackingID inverts BuildTrackingID for every valid
// identity (instance names may be qualified with '_', names may contain ':').
func TestPropertyTrackingIDRoundTrip(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		app := drawApp(t, "cp", "app")
		v := AppInstanceValue{
			ApplicationName: InstanceName(app.Name, app.Namespace, "cp"),
			Namespace:       rapid.OneOf(rapid.Just(""), genDNSLabel).Draw(t, "namespace"),
			Group:           genGroup.Draw(t, "group"),
			Kind:            genKind.Draw(t, "kind"),
			Name:            genObjName.Draw(t, "name"),
		}
		got, err := ParseTrackingID(BuildTrackingID(v))
		require.NoError(t, err)
		require.Equal(t, v, *got)
	})
}

// TestPropertyParseIsCanonical: every accepted string is the tracking id of its parse, and
// every rejected string yields ErrWrongResourceTrackingFormat.
func TestPropertyParseIsCanonical(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		s := rapid.OneOf(rapid.String(), genSeparators).Draw(t, "s")
		v, err := ParseTrackingID(s)
		if err != nil {
			require.ErrorIs(t, err, ErrWrongResourceTrackingFormat)
			require.Nil(t, v)
			return
		}
		require.Equal(t, s, BuildTrackingID(*v))
		require.NotContains(t, v.ApplicationName, ":")
		require.NotContains(t, v.Group+v.Kind, ":")
		require.NotContains(t, v.Group+v.Kind+v.Namespace+v.Name, "/")
	})
}

// TestPropertyTruncateLabel: the result is the longest prefix of the first 63 bytes that ends
// with an alphanumeric character; an error means there is none.
func TestPropertyTruncateLabel(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		s := rapid.OneOf(rapid.String(), genSeparators, rapid.StringMatching(`[a-z0-9_.-]{50,90}`)).Draw(t, "s")
		got, err := TruncateLabel(s)
		if len(s) <= LabelMaxLength {
			require.NoError(t, err)
			require.Equal(t, s, got)
			return
		}
		head := s[:LabelMaxLength]
		last := strings.LastIndexFunc(head, func(r rune) bool {
			return 'a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || '0' <= r && r <= '9'
		})
		if last < 0 {
			require.ErrorIs(t, err, ErrLabelTruncation)
			return
		}
		require.NoError(t, err)
		require.Equal(t, head[:last+1], got)
	})
}

// TestPropertySetAppInstanceRoundTrip: whatever the method, object and Application, the
// tracking metadata written by SetAppInstance is read back as the same Application, refers
// to the object itself, is idempotent and is removed by RemoveAppInstance.
func TestPropertySetAppInstanceRoundTrip(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		cp := genDNSLabel.Draw(t, "controlPlane")
		tr := New(Config{ControlPlaneNamespace: cp})
		m := genMethod.Draw(t, "method")
		inst := genInstallID.Draw(t, "installationID")
		app := drawApp(t, cp, "app")
		g := drawObject(t)
		obj := g.obj
		instance := tr.InstanceName(app)

		require.NoError(t, tr.SetAppInstance(obj, instance, g.defaultNS, m, inst))
		again := obj.DeepCopy()
		require.NoError(t, tr.SetAppInstance(again, instance, g.defaultNS, m, inst))
		require.Equal(t, obj, again, "SetAppInstance must be idempotent")

		require.Equal(t, instance, tr.GetAppName(obj, m, inst))
		require.True(t, tr.IsManagedBy(obj, app, m, inst))
		require.True(t, tr.IsSelfReferencedObject(obj, nil, m, inst))
		require.True(t, tr.IsSelfReferencedObject(obj, obj, m, inst))

		other := drawApp(t, cp, "other")
		if otherInstance := tr.InstanceName(other); otherInstance != instance {
			got, shared := tr.DetectSharedResource(obj, other, m, inst)
			require.True(t, shared)
			require.Equal(t, instance, got)
			require.False(t, tr.IsManagedBy(obj, other, m, inst))
		}

		if UsesAnnotation(m) {
			v := tr.GetAppInstance(obj, m, inst)
			require.NotNil(t, v)
			require.Equal(t, NewAppInstanceValue(obj, instance, g.defaultNS), *v)
			require.Empty(t, tr.GetAppName(obj, m, inst+"-other"), "other installation must not see it")
			require.Equal(t, inst, obj.GetAnnotations()[keyInstallation])
		} else {
			require.Nil(t, tr.GetAppInstance(obj, m, inst))
		}
		if EffectiveMethod(m) == MethodAnnotationAndLabel {
			want, err := TruncateLabel(instance)
			require.NoError(t, err)
			require.Equal(t, want, obj.GetLabels()[keyInstance])
		}

		require.NoError(t, tr.RemoveAppInstance(obj, m))
		require.Empty(t, tr.GetAppName(obj, m, inst))
		require.Empty(t, obj.GetAnnotations())
		require.Empty(t, obj.GetLabels())
	})
}

// TestPropertyCopiedObjectIsNotManaged: an object carrying the tracking id of another
// object (other namespace or name) is attributed to the app but never managed by it.
func TestPropertyCopiedObjectIsNotManaged(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		tr := New(Config{ControlPlaneNamespace: "cp"})
		m := rapid.SampledFrom([]Method{"", MethodAnnotation, MethodAnnotationAndLabel}).Draw(t, "method")
		app := drawApp(t, "cp", "app")
		g := drawObject(t)
		require.NoError(t, tr.SetAppInstance(g.obj, tr.InstanceName(app), g.defaultNS, m, ""))

		cp := g.obj.DeepCopy()
		if g.obj.GetNamespace() != "" && rapid.Bool().Draw(t, "moveNamespace") {
			ns := genDNSLabel.Filter(func(s string) bool { return s != g.obj.GetNamespace() }).Draw(t, "copyNamespace")
			cp.SetNamespace(ns)
		} else {
			cp.SetName(genObjName.Filter(func(s string) bool { return s != g.obj.GetName() }).Draw(t, "copyName"))
		}
		require.Equal(t, tr.InstanceName(app), tr.GetAppName(cp, m, ""))
		require.False(t, tr.IsSelfReferencedObject(cp, nil, m, ""))
		require.False(t, tr.IsManagedBy(cp, app, m, ""))
	})
}

// TestPropertyOwnedObjectIsNotManaged: whatever the method, object and Application, an
// object carrying the app's tracking metadata and at least one ownerReference is attributed
// to the app but is not a managed root (DIFF-076); removing the owners makes it managed again.
func TestPropertyOwnedObjectIsNotManaged(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		cp := genDNSLabel.Draw(t, "controlPlane")
		tr := New(Config{ControlPlaneNamespace: cp})
		m := genMethod.Draw(t, "method")
		inst := genInstallID.Draw(t, "installationID")
		app := drawApp(t, cp, "app")
		g := drawObject(t)
		require.NoError(t, tr.SetAppInstance(g.obj, tr.InstanceName(app), g.defaultNS, m, inst))

		n := rapid.IntRange(1, 3).Draw(t, "owners")
		refs := make([]metav1.OwnerReference, n)
		for i := range refs {
			refs[i] = metav1.OwnerReference{
				APIVersion: "apps/v1",
				Kind:       genKind.Draw(t, "ownerKind"),
				Name:       genObjName.Draw(t, "ownerName"),
				UID:        types.UID(genDNSLabel.Draw(t, "ownerUID")),
			}
		}
		g.obj.SetOwnerReferences(refs)
		require.Equal(t, tr.InstanceName(app), tr.GetAppName(g.obj, m, inst))
		require.True(t, tr.IsSelfReferencedObject(g.obj, nil, m, inst))
		require.False(t, tr.IsManagedBy(g.obj, app, m, inst))

		g.obj.SetOwnerReferences(nil)
		require.True(t, tr.IsManagedBy(g.obj, app, m, inst))
	})
}

// TestPropertyNormalize: Normalize never modifies the target, only touches the instance
// label and the tracking-id annotation of live, never replaces an existing live tracking id,
// is idempotent, and agrees with internal/diff's built-in tracking normalization.
func TestPropertyNormalize(t *testing.T) {
	t.Parallel()
	const custom = "cd.patrol.example/instance"
	values := rapid.SampledFrom([]string{"", "app-a", "app-b", "app-a:/ConfigMap:ns/cm", "app-b:/ConfigMap:ns/cm"})
	rapid.Check(t, func(t *rapid.T) {
		key := rapid.SampledFrom([]string{keyInstance, custom}).Draw(t, "labelKey")
		tr := New(Config{InstanceLabelKey: key})
		m := rapid.SampledFrom(append(allMethods, Method("unknown"))).Draw(t, "method")
		build := func(label string) *unstructured.Unstructured {
			o := configMap("ns", "cm")
			if rapid.Bool().Draw(t, label+"CRD") {
				o = newObj("apiextensions.k8s.io/v1", "CustomResourceDefinition", "", "cm")
			}
			if rapid.Bool().Draw(t, label+"HasLabel") {
				withLabels(o, key, values.Draw(t, label+"Label"))
			}
			if rapid.Bool().Draw(t, label+"HasID") {
				withAnnotations(o, keyTracking, values.Draw(t, label+"ID"))
			}
			if rapid.Bool().Draw(t, label+"Extra") {
				withLabels(o, "extra", "x")
				withAnnotations(o, "extra", "y")
			}
			return o
		}
		target, live := build("target"), build("live")
		origTarget, origLive := target.DeepCopy(), live.DeepCopy()

		require.NoError(t, tr.Normalize(target, live, m))
		require.Equal(t, origTarget, target, "target modified")

		if id := origLive.GetAnnotations()[keyTracking]; id != "" {
			require.Equal(t, id, live.GetAnnotations()[keyTracking], "existing live tracking id replaced")
		}
		strip := func(o *unstructured.Unstructured) *unstructured.Unstructured {
			c := o.DeepCopy()
			ann, lbl := c.GetAnnotations(), c.GetLabels()
			delete(ann, keyTracking)
			delete(lbl, key)
			c.SetAnnotations(nilIfEmpty(ann))
			c.SetLabels(nilIfEmpty(lbl))
			return c
		}
		require.Equal(t, strip(origLive), strip(live), "fields other than tracking metadata changed")

		twice := live.DeepCopy()
		require.NoError(t, tr.Normalize(target, twice, m))
		require.Equal(t, live, twice, "Normalize must be idempotent")

		viaDiff := origLive.DeepCopy()
		lk, ak := tr.DiffTrackingKeys(m)
		require.NoError(t, diff.NormalizeTracking(origTarget.DeepCopy(), viaDiff, diff.TrackingOptions{LabelKey: lk, AnnotationKey: ak}))
		require.Equal(t, live, viaDiff, "tracking.Normalize and diff.NormalizeTracking disagree")
	})
}

// TestPropertyGetAppNameNeverPanics feeds arbitrary metadata shapes to the readers.
func TestPropertyGetAppNameNeverPanics(t *testing.T) {
	t.Parallel()
	// Shapes are built fresh for every draw: SetAppInstance may write into a map shape.
	shapes := []func() any{
		func() any { return nil },
		func() any { return "x" },
		func() any { return int64(1) },
		func() any { return []any{"a"} },
		func() any { return map[string]any{keyTracking: int64(1)} },
		func() any { return map[string]any{keyInstance: true} },
	}
	drawShape := func(t *rapid.T, label string) any {
		return shapes[rapid.IntRange(0, len(shapes)-1).Draw(t, label)]()
	}
	rapid.Check(t, func(t *rapid.T) {
		tr := newTestTracker()
		obj := configMap("ns", "cm")
		md := obj.Object["metadata"].(map[string]any)
		md["labels"] = drawShape(t, "labels")
		md["annotations"] = drawShape(t, "annotations")
		md["ownerReferences"] = drawShape(t, "ownerReferences")
		if rapid.Bool().Draw(t, "brokenMetadata") {
			obj.Object["metadata"] = drawShape(t, "metadata")
		}
		m := genMethod.Draw(t, "method")
		require.Empty(t, tr.GetAppName(obj, m, ""))
		require.Nil(t, tr.GetAppInstance(obj, m, ""))
		require.True(t, tr.IsSelfReferencedObject(obj, nil, m, ""))
		require.False(t, tr.IsManagedBy(obj, App{Name: "app", Namespace: testControlPlane}, m, ""))
		before := obj.DeepCopy()
		if err := tr.SetAppInstance(obj, "app", "ns", m, ""); err != nil {
			require.Equal(t, before, obj, "failed SetAppInstance modified the object")
			require.False(t, errors.Is(err, ErrInvalidInstanceName))
		}
	})
}
