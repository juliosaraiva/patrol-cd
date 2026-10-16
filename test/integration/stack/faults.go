package stack

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
)

// Fault decides whether a request of a component to the control-plane API server fails with
// an injected response (status code, true) instead of reaching the API server.
type Fault func(r *http.Request) (status int, inject bool)

// faultInjector wraps a component's control-plane transport.
type faultInjector struct {
	fault    atomic.Pointer[Fault]
	mu       sync.Mutex
	injected map[int]int
}

func (f *faultInjector) wrap(cfg *rest.Config) *rest.Config {
	out := rest.CopyConfig(cfg)
	out.Wrap(func(rt http.RoundTripper) http.RoundTripper { return &faultTransport{next: rt, f: f} })
	return out
}

func (f *faultInjector) count(code int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.injected == nil {
		f.injected = map[int]int{}
	}
	f.injected[code]++
}

type faultTransport struct {
	next http.RoundTripper
	f    *faultInjector
}

func (t *faultTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if fp := t.f.fault.Load(); fp != nil {
		if code, ok := (*fp)(r); ok {
			t.f.count(code)
			return statusResponse(r, code), nil
		}
	}
	return t.next.RoundTrip(r)
}

// statusResponse is an API server error response (metav1.Status) for code.
func statusResponse(r *http.Request, code int) *http.Response {
	reason := metav1.StatusReasonInternalError
	msg := "injected fault"
	switch code {
	case http.StatusConflict:
		reason = metav1.StatusReasonConflict
		msg = "Operation cannot be fulfilled: the object has been modified; please apply your changes to the latest version and try again (injected)"
	case http.StatusTooManyRequests:
		reason = metav1.StatusReasonTooManyRequests
	case http.StatusServiceUnavailable:
		reason = metav1.StatusReasonServiceUnavailable
	}
	st := metav1.Status{TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"}, Status: metav1.StatusFailure, Message: msg, Reason: reason, Code: int32(code)}
	b, err := json.Marshal(st)
	if err != nil {
		b = []byte(fmt.Sprintf(`{"kind":"Status","apiVersion":"v1","status":"Failure","code":%d}`, code))
	}
	return &http.Response{
		StatusCode: code, Status: fmt.Sprintf("%d %s", code, http.StatusText(code)), Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(bytes.NewReader(b)),
		ContentLength: int64(len(b)), Request: r,
	}
}

// SetControllerFault installs (nil: removes) a fault for the application controller's
// requests to the control-plane API server. It applies to running and restarted
// controllers.
func (s *Stack) SetControllerFault(f Fault) {
	if f == nil {
		s.controllerFaults.fault.Store(nil)
		return
	}
	s.controllerFaults.fault.Store(&f)
}

// InjectedFaults returns how many responses were injected per status code.
func (s *Stack) InjectedFaults() map[int]int {
	s.controllerFaults.mu.Lock()
	defer s.controllerFaults.mu.Unlock()
	out := make(map[int]int, len(s.controllerFaults.injected))
	for k, v := range s.controllerFaults.injected {
		out[k] = v
	}
	return out
}

// IsWrite reports whether r changes an object of the given resource (plural, e.g.
// "operations") in the Patrol API group, optionally only its status subresource.
func IsWrite(r *http.Request, resource string, statusOnly bool) bool {
	switch r.Method {
	case http.MethodPut, http.MethodPatch:
	default:
		return false
	}
	p := r.URL.Path
	if !strings.Contains(p, "/apis/cd.patrol.example/") || !strings.Contains(p, "/"+resource+"/") {
		return false
	}
	return !statusOnly || strings.HasSuffix(p, "/status")
}
