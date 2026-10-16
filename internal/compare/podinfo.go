package compare

import (
	"fmt"
	"maps"
	"slices"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

// populatePodInfo computes the kubectl-style status reason, container counts, restarts,
// images and requests of a Pod (upstream populatePodInfo, DIFF-089).
func populatePodInfo(un *unstructured.Unstructured, res *ResourceInfo) {
	var pod corev1.Pod
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(un.Object, &pod); err != nil {
		return
	}
	restarts := 0
	total := len(pod.Spec.Containers)
	ready := 0
	phase := pod.Status.Phase
	reason := string(phase)
	if pod.Status.Reason != "" {
		reason = pod.Status.Reason
	}

	images := map[string]bool{}
	for _, c := range pod.Spec.InitContainers {
		images[c.Image] = true
	}
	for _, c := range pod.Spec.Containers {
		images[c.Image] = true
	}
	res.Images = slices.Sorted(maps.Keys(images))

	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Reason == corev1.PodReasonSchedulingGated {
			reason = corev1.PodReasonSchedulingGated
		}
	}

	initContainers := map[string]*corev1.Container{}
	for i := range pod.Spec.InitContainers {
		ic := &pod.Spec.InitContainers[i]
		initContainers[ic.Name] = ic
		if isRestartableInit(ic) {
			total++
		}
	}

	initializing := false
	for i, cs := range pod.Status.InitContainerStatuses {
		restarts += int(cs.RestartCount)
		switch {
		case cs.State.Terminated != nil && cs.State.Terminated.ExitCode == 0:
			continue
		case isRestartableInit(initContainers[cs.Name]) && cs.Started != nil && *cs.Started:
			if cs.Ready {
				ready++
			}
			continue
		case cs.State.Terminated != nil:
			switch {
			case cs.State.Terminated.Reason != "":
				reason = "Init:" + cs.State.Terminated.Reason
			case cs.State.Terminated.Signal != 0:
				reason = fmt.Sprintf("Init:Signal:%d", cs.State.Terminated.Signal)
			default:
				reason = fmt.Sprintf("Init:ExitCode:%d", cs.State.Terminated.ExitCode)
			}
			initializing = true
		case cs.State.Waiting != nil && cs.State.Waiting.Reason != "" && cs.State.Waiting.Reason != "PodInitializing":
			reason = "Init:" + cs.State.Waiting.Reason
			initializing = true
		default:
			reason = fmt.Sprintf("Init:%d/%d", i, len(pod.Spec.InitContainers))
			initializing = true
		}
		break
	}
	if !initializing || podInitialized(&pod.Status) {
		hasRunning := false
		for i := len(pod.Status.ContainerStatuses) - 1; i >= 0; i-- {
			cs := pod.Status.ContainerStatuses[i]
			restarts += int(cs.RestartCount)
			switch {
			case cs.State.Waiting != nil && cs.State.Waiting.Reason != "":
				reason = cs.State.Waiting.Reason
			case cs.State.Terminated != nil && cs.State.Terminated.Reason != "":
				reason = cs.State.Terminated.Reason
			case cs.State.Terminated != nil && cs.State.Terminated.Signal != 0:
				reason = fmt.Sprintf("Signal:%d", cs.State.Terminated.Signal)
			case cs.State.Terminated != nil:
				reason = fmt.Sprintf("ExitCode:%d", cs.State.Terminated.ExitCode)
			case cs.Ready && cs.State.Running != nil:
				hasRunning = true
				ready++
			}
		}
		if reason == "Completed" && hasRunning {
			reason = "Running"
		}
	}

	// "NodeLost" is the node controller's reason for pods on unreachable nodes.
	if pod.DeletionTimestamp != nil && pod.Status.Reason == "NodeLost" {
		reason = "Unknown"
	} else if pod.DeletionTimestamp != nil && !podTerminal(phase) {
		reason = "Terminating"
	}
	if reason != "" {
		res.Info = append(res.Info, InfoItem{Name: InfoStatusReason, Value: reason})
	}

	req := podRequests(&pod)
	res.PodInfo = &PodInfo{NodeName: pod.Spec.NodeName, ResourceRequests: req, Phase: phase}
	res.Info = append(res.Info,
		InfoItem{Name: InfoNode, Value: pod.Spec.NodeName},
		InfoItem{Name: InfoContainers, Value: fmt.Sprintf("%d/%d", ready, total)},
	)
	if restarts > 0 {
		res.Info = append(res.Info, InfoItem{Name: InfoRestartCount, Value: strconv.Itoa(restarts)})
	}
	if !podTerminal(phase) {
		cpu := req[corev1.ResourceCPU]
		mem := req[corev1.ResourceMemory]
		res.Info = append(res.Info,
			InfoItem{Name: InfoRequestsCPU, Value: strconv.FormatInt(cpu.MilliValue(), 10)},
			InfoItem{Name: InfoRequestsMem, Value: strconv.FormatInt(mem.MilliValue(), 10)},
		)
	}
	res.NetworkingInfo = &NetworkingInfo{Labels: un.GetLabels(), ExternalURLs: existingURLs(res)}
}

func isRestartableInit(c *corev1.Container) bool {
	return c != nil && c.RestartPolicy != nil && *c.RestartPolicy == corev1.ContainerRestartPolicyAlways
}

func podInitialized(st *corev1.PodStatus) bool {
	for _, c := range st.Conditions {
		if c.Type == corev1.PodInitialized {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func podTerminal(p corev1.PodPhase) bool {
	return p == corev1.PodSucceeded || p == corev1.PodFailed
}

// podRequests computes the effective resource requests of a pod like the kubelet and the
// scheduler (k8s.io/component-helpers PodRequests with UseStatusResources): per container the
// larger of the spec and the reported (resized) requests; restartable init containers
// (sidecars) run alongside the regular containers; a regular init container needs its own
// requests plus the sidecars started before it; pod-level requests replace the aggregate for
// the resources they set; the pod overhead is added.
func podRequests(pod *corev1.Pod) corev1.ResourceList {
	statusReqs := map[string]corev1.ResourceList{}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Resources != nil {
			statusReqs[cs.Name] = cs.Resources.Requests
		}
	}
	for _, cs := range pod.Status.InitContainerStatuses {
		if cs.Resources != nil {
			statusReqs[cs.Name] = cs.Resources.Requests
		}
	}
	effective := func(c *corev1.Container) corev1.ResourceList {
		out := c.Resources.Requests.DeepCopy()
		if out == nil {
			out = corev1.ResourceList{}
		}
		if st, ok := statusReqs[c.Name]; ok {
			maxInto(out, st)
		}
		return out
	}

	reqs := corev1.ResourceList{}
	for i := range pod.Spec.Containers {
		addInto(reqs, effective(&pod.Spec.Containers[i]))
	}
	sidecars := corev1.ResourceList{}
	initMax := corev1.ResourceList{}
	for i := range pod.Spec.InitContainers {
		c := &pod.Spec.InitContainers[i]
		r := effective(c)
		if isRestartableInit(c) {
			addInto(reqs, r)
			addInto(sidecars, r)
			maxInto(initMax, sidecars)
			continue
		}
		step := sidecars.DeepCopy()
		addInto(step, r)
		maxInto(initMax, step)
	}
	maxInto(reqs, initMax)

	if pod.Spec.Resources != nil {
		for name, q := range pod.Spec.Resources.Requests {
			if name == corev1.ResourceCPU || name == corev1.ResourceMemory {
				reqs[name] = q.DeepCopy()
			}
		}
	}
	addInto(reqs, pod.Spec.Overhead)
	return reqs
}

func addInto(dst, src corev1.ResourceList) {
	for name, q := range src {
		if cur, ok := dst[name]; ok {
			cur.Add(q)
			dst[name] = cur
		} else {
			dst[name] = q.DeepCopy()
		}
	}
}

func maxInto(dst, src corev1.ResourceList) {
	for name, q := range src {
		if cur, ok := dst[name]; !ok || q.Cmp(cur) > 0 {
			dst[name] = q.DeepCopy()
		}
	}
}
