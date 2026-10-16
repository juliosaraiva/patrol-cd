package projects

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode"

	"github.com/gobwas/glob"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	"github.com/juliosaraiva/patrol-cd/internal/project"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
)

var errNotConfigured = errors.New("not configured")

func (s *Service) ready() error {
	if s.d.Client == nil || s.d.Config.Namespace == "" {
		return rest.Unavailable("the project API is not configured").WithCause(errNotConfigured)
	}
	return nil
}

func (s *Service) settings() (*settings.Settings, error) {
	if s.d.Settings == nil {
		return nil, rest.Unavailable("settings are not available").WithCause(errNotConfigured)
	}
	st := s.d.Settings.Current()
	if st == nil {
		return nil, rest.Unavailable("settings are not loaded yet")
	}
	return st, nil
}

// getProject reads a project of the control-plane namespace (Kubernetes NotFound when
// missing, mapped to 404).
func (s *Service) getProject(ctx context.Context, name string) (*v1alpha1.AppProject, error) {
	var p v1alpha1.AppProject
	if err := s.d.Client.Get(ctx, client.ObjectKey{Namespace: s.d.Config.Namespace, Name: name}, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// GetProject implements project.ProjectGetter (global projects).
func (s *Service) GetProject(ctx context.Context, name string) (*v1alpha1.AppProject, error) {
	return s.getProject(ctx, name)
}

// globalSettings converts the globalProjects setting.
func globalSettings(st *settings.Settings) []project.GlobalProjectSetting {
	var out []project.GlobalProjectSetting
	for _, g := range st.GlobalProjects() {
		out = append(out, project.GlobalProjectSetting{ProjectName: g.ProjectName, LabelSelector: *g.LabelSelector.DeepCopy()})
	}
	return out
}

// validationError maps an internal/project validation error to an API error with the
// upstream code and message (prefix prepended).
func validationError(err error, prefix string) error {
	var ve *project.ValidationError
	if !errors.As(err, &ve) {
		return rest.InvalidArgument("%s%v", prefix, err)
	}
	msg := prefix + ve.Message
	switch ve.Code {
	case project.CodeAlreadyExists:
		return rest.AlreadyExists("%s", msg)
	case project.CodeNotFound:
		return rest.NotFound("%s", msg)
	case project.CodeInternal:
		return rest.Internal(err)
	default:
		return rest.InvalidArgument("%s", msg)
	}
}

// maxSourceNamespaceLength bounds a sourceNamespaces entry.
const maxSourceNamespaceLength = 253

// validateSourceNamespaces checks that every spec.sourceNamespaces entry is a usable glob or
// "/regex/" (RE2): internal/project silently never matches a malformed entry, which would
// hide a typo; the API refuses it instead.
func validateSourceNamespaces(entries []string) error {
	for _, ns := range entries {
		switch {
		case ns == "":
			return rest.InvalidArgument("source namespace must not be empty")
		case len(ns) > maxSourceNamespaceLength:
			return rest.InvalidArgument("source namespace '%s' is longer than %d characters", ns, maxSourceNamespaceLength)
		case strings.IndexFunc(ns, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0:
			return rest.InvalidArgument("source namespace '%s' must not contain whitespace", ns)
		}
		if len(ns) >= 2 && strings.HasPrefix(ns, "/") && strings.HasSuffix(ns, "/") {
			if _, err := regexp.Compile(ns[1 : len(ns)-1]); err != nil {
				return rest.InvalidArgument("source namespace '%s' is not a valid regular expression: %v", ns, err)
			}
			continue
		}
		if _, err := glob.Compile(ns); err != nil {
			return rest.InvalidArgument("source namespace '%s' is not a valid pattern: %v", ns, err)
		}
	}
	return nil
}

// validateName checks a project name (an object name).
func validateName(name string) error {
	if name == "" {
		return rest.InvalidArgument("project name is required")
	}
	if errs := validation.IsDNS1123Subdomain(name); len(errs) > 0 {
		return rest.InvalidArgument("invalid project name '%s': %s", name, strings.Join(errs, "; "))
	}
	return nil
}

// keyLock serializes mutations of one project inside this process (upstream projectLock);
// resourceVersion preconditions protect against other writers.
type keyLock struct {
	mu sync.Mutex
	m  map[string]*lockEntry
}

type lockEntry struct {
	mu   sync.Mutex
	refs int
}

func newKeyLock() *keyLock { return &keyLock{m: map[string]*lockEntry{}} }

// Lock locks key and returns the unlock function.
func (k *keyLock) Lock(key string) func() {
	k.mu.Lock()
	e := k.m[key]
	if e == nil {
		e = &lockEntry{}
		k.m[key] = e
	}
	e.refs++
	k.mu.Unlock()
	e.mu.Lock()
	return func() {
		e.mu.Unlock()
		k.mu.Lock()
		e.refs--
		if e.refs == 0 {
			delete(k.m, key)
		}
		k.mu.Unlock()
	}
}

// symmetricDifference returns the values present in exactly one of a and b, sorted (as
// sets: upstream counts occurrences, so a value listed twice in one list and absent from
// the other escaped the permission check).
func symmetricDifference(a, b []string) []string {
	inA, inB := map[string]bool{}, map[string]bool{}
	for _, v := range a {
		inA[v] = true
	}
	for _, v := range b {
		inB[v] = true
	}
	var out []string
	for v := range inA {
		if !inB[v] {
			out = append(out, v)
		}
	}
	for v := range inB {
		if !inA[v] {
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

// diffPaths lists the paths of the fields that differ between a and b, flattened as
// upstream GenerateSpecIsDifferentErrorMessage does (Go field names, slice indices and
// map keys). Nil and empty slices and maps are equal.
func diffPaths(a, b any) []string {
	var out []string
	walkDiff(reflect.ValueOf(a), reflect.ValueOf(b), nil, &out)
	return out
}

func walkDiff(a, b reflect.Value, path []string, out *[]string) {
	add := func() {
		if len(path) == 0 {
			*out = append(*out, "(root)")
			return
		}
		*out = append(*out, path...)
	}
	if !a.IsValid() || !b.IsValid() {
		if a.IsValid() != b.IsValid() {
			add()
		}
		return
	}
	switch a.Kind() {
	case reflect.Pointer, reflect.Interface:
		if a.IsNil() || b.IsNil() {
			if a.IsNil() != b.IsNil() {
				add()
			}
			return
		}
		walkDiff(a.Elem(), b.Elem(), path, out)
	case reflect.Struct:
		t := a.Type()
		for i := range t.NumField() {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			walkDiff(a.Field(i), b.Field(i), append(slices.Clip(path), f.Name), out)
		}
	case reflect.Slice:
		n := max(a.Len(), b.Len())
		for i := range n {
			p := append(slices.Clip(path), strconv.Itoa(i))
			if i >= a.Len() || i >= b.Len() {
				*out = append(*out, p...)
				continue
			}
			walkDiff(a.Index(i), b.Index(i), p, out)
		}
	case reflect.Map:
		keys := map[string]reflect.Value{}
		for _, k := range a.MapKeys() {
			keys[fmt.Sprint(k.Interface())] = k
		}
		for _, k := range b.MapKeys() {
			keys[fmt.Sprint(k.Interface())] = k
		}
		names := make([]string, 0, len(keys))
		for n := range keys {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			k := keys[n]
			walkDiff(a.MapIndex(k), b.MapIndex(k), append(slices.Clip(path), n), out)
		}
	default:
		if !reflect.DeepEqual(a.Interface(), b.Interface()) {
			add()
		}
	}
}

// encodeContinue and decodeContinue implement the opaque list continuation token (the
// last returned name).
func encodeContinue(key string) string { return base64.RawURLEncoding.EncodeToString([]byte(key)) }

func decodeContinue(tok string) (string, error) {
	b, err := base64.RawURLEncoding.DecodeString(tok)
	if err != nil || len(b) == 0 {
		return "", rest.InvalidArgument("invalid continue token")
	}
	return string(b), nil
}

// paginate returns the items after the continue key, at most limit of them (0: all), and
// the continue token of the next page. items must be sorted by key.
func paginate[T any](items []T, key func(T) string, limit int64, cont string) ([]T, string, error) {
	if limit < 0 {
		return nil, "", rest.InvalidArgument("limit must not be negative")
	}
	start := 0
	if cont != "" {
		after, err := decodeContinue(cont)
		if err != nil {
			return nil, "", err
		}
		start = len(items)
		for i, it := range items {
			if key(it) > after {
				start = i
				break
			}
		}
	}
	items = items[start:]
	if limit == 0 || int64(len(items)) <= limit {
		return items, "", nil
	}
	page := items[:limit]
	return page, encodeContinue(key(page[len(page)-1])), nil
}
