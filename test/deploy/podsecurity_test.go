package deploy_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
)

// podTemplate is one pod template of a rendered workload.
type podTemplate struct {
	owner string // kind/namespace/name of the workload
	tmpl  corev1.PodTemplateSpec
}

// podTemplates returns the pod templates of the rendered Deployments, StatefulSets,
// DaemonSets, Jobs and CronJobs.
func podTemplates(t testing.TB, objs []object) []podTemplate {
	t.Helper()
	var out []podTemplate
	for _, o := range objs {
		var tmpl *corev1.PodTemplateSpec
		switch o.u.GetKind() {
		case "Deployment":
			var d appsv1.Deployment
			fromUnstructured(t, o, &d)
			tmpl = &d.Spec.Template
		case "StatefulSet":
			var s appsv1.StatefulSet
			fromUnstructured(t, o, &s)
			tmpl = &s.Spec.Template
		case "DaemonSet":
			var d appsv1.DaemonSet
			fromUnstructured(t, o, &d)
			tmpl = &d.Spec.Template
		case "Job":
			var j batchv1.Job
			fromUnstructured(t, o, &j)
			tmpl = &j.Spec.Template
		case "CronJob":
			var c batchv1.CronJob
			fromUnstructured(t, o, &c)
			tmpl = &c.Spec.JobTemplate.Spec.Template
		}
		if tmpl != nil {
			out = append(out, podTemplate{owner: o.id(), tmpl: *tmpl})
		}
	}
	return out
}

// fromUnstructured converts strictly (unknown fields fail).
func fromUnstructured(t testing.TB, o object, into any) {
	t.Helper()
	if err := runtime.DefaultUnstructuredConverter.FromUnstructuredWithValidation(o.u.Object, into, true); err != nil {
		t.Fatalf("%s: %v", o.id(), err)
	}
}

// restrictedLabels enforce, audit and warn the "restricted" Pod Security level.
var restrictedLabels = map[string]string{
	"pod-security.kubernetes.io/enforce":         "restricted",
	"pod-security.kubernetes.io/enforce-version": "latest",
	"pod-security.kubernetes.io/warn":            "restricted",
	"pod-security.kubernetes.io/warn-version":    "latest",
	"pod-security.kubernetes.io/audit":           "restricted",
	"pod-security.kubernetes.io/audit-version":   "latest",
}

// podFor builds the Pod a controller would create from a template.
func podFor(pt podTemplate, namespace, name string) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: *pt.tmpl.ObjectMeta.DeepCopy(),
		Spec:       *pt.tmpl.Spec.DeepCopy(),
	}
	pod.Name, pod.Namespace, pod.GenerateName = name, namespace, ""
	return pod
}

// TestPodSecurityRestricted creates (dry run) a Pod from every pod template of every profile
// in a namespace enforcing the "restricted" Pod Security level. The PodSecurity admission
// plugin is enabled by default in kube-apiserver (envtest disables only ServiceAccount); the
// negative control proves it enforces here.
func TestPodSecurityRestricted(t *testing.T) {
	t.Parallel()
	k := sharedEnv(t)
	const ns = "pss-restricted"
	setupCtx, cancelSetup := context.WithTimeout(context.Background(), time.Minute)
	err := k.ensureNamespace(setupCtx, ns, restrictedLabels)
	cancelSetup()
	if err != nil {
		t.Fatal(err)
	}

	t.Run("negative-control", func(t *testing.T) {
		// Its own deadline: it runs now, and must not share one with the profile subtests below.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		kube, warns := k.warningClient(t)
		bad := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "privileged", Namespace: ns},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name: "c", Image: "busybox:1.37.0",
				SecurityContext: &corev1.SecurityContext{Privileged: ptr.To(true)},
			}}},
		}
		_, err := kube.CoreV1().Pods(ns).Create(ctx, bad, metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}})
		if !apierrors.IsForbidden(err) || !strings.Contains(err.Error(), `violates PodSecurity "restricted:latest"`) {
			t.Fatalf("privileged pod: want a PodSecurity rejection, got %v", err)
		}
		// A pod that is only "baseline" (no seccomp, root allowed) is rejected too.
		baseline := bad.DeepCopy()
		baseline.Name = "baseline"
		baseline.Spec.Containers[0].SecurityContext = nil
		_, err = kube.CoreV1().Pods(ns).Create(ctx, baseline, metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}})
		if !apierrors.IsForbidden(err) || !strings.Contains(err.Error(), "runAsNonRoot") {
			t.Fatalf("baseline pod: want a PodSecurity rejection naming runAsNonRoot, got %v", err)
		}
		warns.take()
	})

	all := profiles(t)
	var mu sync.Mutex
	profilesRun, pods, containers := 0, 0, 0
	t.Cleanup(func() {
		// Runs after the parallel subtests. A suite that admitted nothing, or skipped a profile,
		// proved nothing about Pod Security.
		t.Logf("Pod Security restricted: %d profiles, %d pods (%d containers incl. init) admitted", profilesRun, pods, containers)
		if !t.Failed() && (profilesRun != len(all) || pods == 0) {
			t.Errorf("Pod Security restricted exercised %d of %d profiles and admitted %d pods", profilesRun, len(all), pods)
		}
	})
	for _, p := range all {
		t.Run(p.name, func(t *testing.T) {
			t.Parallel()
			// A parallel subtest starts when a -parallel slot frees up, which is minutes after its
			// parent on a 2-CPU runner (two slots, long tests ahead of it). Its deadline starts
			// here: the client's rate limiter fails a request at once when the deadline left is
			// shorter than its wait ("client rate limiter Wait returned an error").
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			kube, warns := k.warningClient(t)
			admitted, admittedContainers := 0, 0
			for i, pt := range podTemplates(t, renderProfile(t, p)) {
				pod := podFor(pt, ns, fmt.Sprintf("%s-%d", p.name, i))
				warns.take()
				_, err := kube.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{
					DryRun: []string{metav1.DryRunAll}, FieldValidation: metav1.FieldValidationStrict,
				})
				if err != nil {
					t.Errorf("%s: pod rejected: %v", pt.owner, err)
					continue
				}
				if ws := warns.take(); len(ws) > 0 {
					t.Errorf("%s: warnings:\n%s", pt.owner, joinLines(ws))
					continue
				}
				admitted++
				admittedContainers += len(pod.Spec.InitContainers) + len(pod.Spec.Containers)
			}
			mu.Lock()
			profilesRun++
			pods += admitted
			containers += admittedContainers
			mu.Unlock()
		})
	}
}
