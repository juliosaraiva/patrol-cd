package transport

import (
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestDirectAndUnavailable(t *testing.T) {
	t.Parallel()
	if _, err := Direct.Connect(t.Context(), Request{}); err == nil || err.Error() != "Patrol server address unspecified" {
		t.Fatalf("empty address: %v", err)
	}
	c, err := Direct.Connect(t.Context(), Request{ServerAddr: "cd.example.com:443", Insecure: true})
	if err != nil || c.ServerAddr != "cd.example.com:443" || !c.Insecure || c.Close() != nil {
		t.Fatalf("direct: %+v %v", c, err)
	}
	if _, err := Unavailable(ErrCoreModeUnavailable).Connect(t.Context(), Request{}); !errors.Is(err, ErrCoreModeUnavailable) {
		t.Fatalf("unavailable: %v", err)
	}
	closed := 0
	conn := NewConnection(Connection{}, func() error { closed++; return nil })
	_ = conn.Close()
	_ = conn.Close()
	if closed != 1 {
		t.Fatalf("close called %d times", closed)
	}
	var nilConn *Connection
	if nilConn.Close() != nil {
		t.Fatal("nil close")
	}
}

func pod(name string, phase corev1.PodPhase, ready bool, deleting bool) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "patrol", Labels: map[string]string{LabelAppName: "patrol-server"}},
		Status:     corev1.PodStatus{Phase: phase},
	}
	st := corev1.ConditionFalse
	if ready {
		st = corev1.ConditionTrue
	}
	p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: st}}
	if deleting {
		now := metav1.Now()
		p.DeletionTimestamp = &now
		p.Finalizers = []string{"x"}
	}
	return p
}

func TestSelectReadyPod(t *testing.T) {
	t.Parallel()
	cs := fake.NewClientset(
		pod("pending", corev1.PodPending, false, false),
		pod("unready", corev1.PodRunning, false, false),
		pod("terminating", corev1.PodRunning, true, true),
		pod("good", corev1.PodRunning, true, false),
	)
	p, err := selectReadyPod(t.Context(), cs, "patrol", LabelAppName+"=patrol-server")
	if err != nil || p.Name != "good" {
		t.Fatalf("selected %v, %v", p, err)
	}
	_, err = selectReadyPod(t.Context(), cs, "other", LabelAppName+"=patrol-server")
	if err == nil || !strings.Contains(err.Error(), "cannot find ready pod with selector: [app.kubernetes.io/name=patrol-server] in namespace other") {
		t.Fatalf("no pod: %v", err)
	}
}
