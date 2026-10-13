package diff

import (
	"reflect"
	"strings"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	admissionregistrationv1beta1 "k8s.io/api/admissionregistration/v1beta1"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
)

// applyDefaults sets the API server's static defaults on a typed built-in object, mirroring
// the defaulting functions of the Kubernetes API (pkg/apis/*/v1/defaults.go) for the kinds
// most commonly managed by GitOps. The baseline links the full Kubernetes defaulters; Patrol
// does not depend on k8s.io/kubernetes, so this covers a curated subset (see the component
// documentation for the list). Unknown types are left untouched.
func applyDefaults(obj runtime.Object) {
	switch t := obj.(type) {
	case *corev1.Pod:
		defaultPod(t)
	case *corev1.PodTemplate:
		defaultPodTemplateSpec(&t.Template)
	case *corev1.ReplicationController:
		defaultReplicationController(t)
	case *corev1.Service:
		defaultService(t)
	case *corev1.Secret:
		if t.Type == "" {
			t.Type = corev1.SecretTypeOpaque
		}
	case *corev1.Endpoints: //nolint:staticcheck // still a valid managed resource
		for i := range t.Subsets {
			for j := range t.Subsets[i].Ports {
				if t.Subsets[i].Ports[j].Protocol == "" {
					t.Subsets[i].Ports[j].Protocol = corev1.ProtocolTCP
				}
			}
		}
	case *corev1.PersistentVolumeClaim:
		defaultPVC(t)
	case *corev1.Namespace:
		if t.Name != "" {
			if t.Labels == nil {
				t.Labels = map[string]string{}
			}
			t.Labels[corev1.LabelMetadataName] = t.Name
		}
	case *appsv1.Deployment:
		defaultDeployment(t)
	case *appsv1.StatefulSet:
		defaultStatefulSet(t)
	case *appsv1.DaemonSet:
		defaultDaemonSet(t)
	case *appsv1.ReplicaSet:
		if t.Spec.Replicas == nil {
			t.Spec.Replicas = ptr.To[int32](1)
		}
		defaultPodTemplateSpec(&t.Spec.Template)
	case *batchv1.Job:
		defaultJob(t)
	case *batchv1.CronJob:
		defaultCronJob(t)
	case *networkingv1.NetworkPolicy:
		defaultNetworkPolicy(t)
	case *autoscalingv1.HorizontalPodAutoscaler:
		if t.Spec.MinReplicas == nil {
			t.Spec.MinReplicas = ptr.To[int32](1)
		}
	case *autoscalingv2.HorizontalPodAutoscaler:
		defaultHPAv2(t)
	case *admissionregistrationv1.MutatingWebhookConfiguration:
		for i := range t.Webhooks {
			w := &t.Webhooks[i]
			defaultWebhookCommon(&w.FailurePolicy, &w.MatchPolicy, &w.NamespaceSelector, &w.ObjectSelector, &w.TimeoutSeconds, w.Rules, &w.ClientConfig)
			if w.ReinvocationPolicy == nil {
				w.ReinvocationPolicy = ptr.To(admissionregistrationv1.NeverReinvocationPolicy)
			}
		}
	case *admissionregistrationv1.ValidatingWebhookConfiguration:
		for i := range t.Webhooks {
			w := &t.Webhooks[i]
			defaultWebhookCommon(&w.FailurePolicy, &w.MatchPolicy, &w.NamespaceSelector, &w.ObjectSelector, &w.TimeoutSeconds, w.Rules, &w.ClientConfig)
		}
	case *admissionregistrationv1beta1.MutatingWebhookConfiguration:
		for i := range t.Webhooks {
			w := &t.Webhooks[i]
			defaultWebhookV1beta1(&w.FailurePolicy, &w.MatchPolicy, &w.NamespaceSelector, &w.ObjectSelector, &w.SideEffects, &w.TimeoutSeconds, &w.AdmissionReviewVersions, w.Rules, &w.ClientConfig)
			if w.ReinvocationPolicy == nil {
				w.ReinvocationPolicy = ptr.To(admissionregistrationv1beta1.NeverReinvocationPolicy)
			}
		}
	case *admissionregistrationv1beta1.ValidatingWebhookConfiguration:
		for i := range t.Webhooks {
			w := &t.Webhooks[i]
			defaultWebhookV1beta1(&w.FailurePolicy, &w.MatchPolicy, &w.NamespaceSelector, &w.ObjectSelector, &w.SideEffects, &w.TimeoutSeconds, &w.AdmissionReviewVersions, w.Rules, &w.ClientConfig)
		}
	case *storagev1.StorageClass:
		if t.ReclaimPolicy == nil {
			t.ReclaimPolicy = ptr.To(corev1.PersistentVolumeReclaimDelete)
		}
		if t.VolumeBindingMode == nil {
			t.VolumeBindingMode = ptr.To(storagev1.VolumeBindingImmediate)
		}
	case *schedulingv1.PriorityClass:
		if t.PreemptionPolicy == nil {
			t.PreemptionPolicy = ptr.To(corev1.PreemptLowerPriority)
		}
	}
}

func defaultPod(p *corev1.Pod) {
	defaultPodSpec(&p.Spec)
	for _, cs := range [][]corev1.Container{p.Spec.Containers, p.Spec.InitContainers} {
		for i := range cs {
			r := &cs[i].Resources
			if r.Limits == nil {
				continue
			}
			if r.Requests == nil {
				r.Requests = corev1.ResourceList{}
			}
			for k, v := range r.Limits {
				if _, ok := r.Requests[k]; !ok {
					r.Requests[k] = v.DeepCopy()
				}
			}
		}
	}
	if p.Spec.EnableServiceLinks == nil {
		p.Spec.EnableServiceLinks = ptr.To(corev1.DefaultEnableServiceLinks)
	}
	if p.Spec.HostNetwork {
		defaultHostNetworkPorts(p.Spec.Containers)
		defaultHostNetworkPorts(p.Spec.InitContainers)
	}
}

func defaultHostNetworkPorts(cs []corev1.Container) {
	for i := range cs {
		for j := range cs[i].Ports {
			if cs[i].Ports[j].HostPort == 0 {
				cs[i].Ports[j].HostPort = cs[i].Ports[j].ContainerPort
			}
		}
	}
}

func defaultPodTemplateSpec(t *corev1.PodTemplateSpec) {
	defaultPodSpec(&t.Spec)
}

func defaultPodSpec(s *corev1.PodSpec) {
	if s.DNSPolicy == "" {
		s.DNSPolicy = corev1.DNSClusterFirst
	}
	if s.RestartPolicy == "" {
		s.RestartPolicy = corev1.RestartPolicyAlways
	}
	if s.SecurityContext == nil {
		s.SecurityContext = &corev1.PodSecurityContext{}
	}
	if s.TerminationGracePeriodSeconds == nil {
		s.TerminationGracePeriodSeconds = ptr.To[int64](corev1.DefaultTerminationGracePeriodSeconds)
	}
	if s.SchedulerName == "" {
		s.SchedulerName = corev1.DefaultSchedulerName
	}
	for i := range s.Containers {
		defaultContainer(&s.Containers[i])
	}
	for i := range s.InitContainers {
		defaultContainer(&s.InitContainers[i])
	}
	for i := range s.EphemeralContainers {
		c := corev1.Container(s.EphemeralContainers[i].EphemeralContainerCommon)
		defaultContainer(&c)
		s.EphemeralContainers[i].EphemeralContainerCommon = corev1.EphemeralContainerCommon(c)
	}
	for i := range s.Volumes {
		defaultVolume(&s.Volumes[i])
	}
	roundResourceList(s.Overhead)
	if s.Resources != nil {
		roundResourceList(s.Resources.Limits)
		roundResourceList(s.Resources.Requests)
	}
}

func defaultContainer(c *corev1.Container) {
	if c.ImagePullPolicy == "" {
		if imageTag(c.Image) == "latest" {
			c.ImagePullPolicy = corev1.PullAlways
		} else {
			c.ImagePullPolicy = corev1.PullIfNotPresent
		}
	}
	if c.TerminationMessagePath == "" {
		c.TerminationMessagePath = corev1.TerminationMessagePathDefault
	}
	if c.TerminationMessagePolicy == "" {
		c.TerminationMessagePolicy = corev1.TerminationMessageReadFile
	}
	for i := range c.Ports {
		if c.Ports[i].Protocol == "" {
			c.Ports[i].Protocol = corev1.ProtocolTCP
		}
	}
	for i := range c.Env {
		if c.Env[i].ValueFrom != nil {
			defaultFieldRef(c.Env[i].ValueFrom.FieldRef)
		}
	}
	for _, p := range []*corev1.Probe{c.LivenessProbe, c.ReadinessProbe, c.StartupProbe} {
		defaultProbe(p)
	}
	if c.Lifecycle != nil {
		for _, h := range []*corev1.LifecycleHandler{c.Lifecycle.PostStart, c.Lifecycle.PreStop} {
			if h != nil {
				defaultHTTPGet(h.HTTPGet)
			}
		}
	}
	roundResourceList(c.Resources.Limits)
	roundResourceList(c.Resources.Requests)
}

// imageTag returns the tag the kubelet would resolve for image: "latest" when neither a tag
// nor a digest is given, "" for digest-only references.
func imageTag(image string) string {
	name, digested := image, false
	if i := strings.Index(name, "@"); i >= 0 {
		name, digested = name[:i], true
	}
	last := name
	if i := strings.LastIndex(name, "/"); i >= 0 {
		last = name[i+1:]
	}
	if i := strings.LastIndex(last, ":"); i >= 0 {
		return last[i+1:]
	}
	if digested {
		return ""
	}
	return "latest"
}

func defaultProbe(p *corev1.Probe) {
	if p == nil {
		return
	}
	if p.TimeoutSeconds == 0 {
		p.TimeoutSeconds = 1
	}
	if p.PeriodSeconds == 0 {
		p.PeriodSeconds = 10
	}
	if p.SuccessThreshold == 0 {
		p.SuccessThreshold = 1
	}
	if p.FailureThreshold == 0 {
		p.FailureThreshold = 3
	}
	defaultHTTPGet(p.HTTPGet)
}

func defaultHTTPGet(h *corev1.HTTPGetAction) {
	if h == nil {
		return
	}
	if h.Path == "" {
		h.Path = "/"
	}
	if h.Scheme == "" {
		h.Scheme = corev1.URISchemeHTTP
	}
}

func defaultFieldRef(f *corev1.ObjectFieldSelector) {
	if f != nil && f.APIVersion == "" {
		f.APIVersion = "v1"
	}
}

func defaultVolume(v *corev1.Volume) {
	if volumeSourceEmpty(&v.VolumeSource) {
		v.EmptyDir = &corev1.EmptyDirVolumeSource{}
	}
	const mode = corev1.SecretVolumeSourceDefaultMode
	if v.Secret != nil && v.Secret.DefaultMode == nil {
		v.Secret.DefaultMode = ptr.To(mode)
	}
	if v.ConfigMap != nil && v.ConfigMap.DefaultMode == nil {
		v.ConfigMap.DefaultMode = ptr.To(mode)
	}
	if v.DownwardAPI != nil {
		if v.DownwardAPI.DefaultMode == nil {
			v.DownwardAPI.DefaultMode = ptr.To(mode)
		}
		for i := range v.DownwardAPI.Items {
			defaultFieldRef(v.DownwardAPI.Items[i].FieldRef)
		}
	}
	if v.Projected != nil {
		if v.Projected.DefaultMode == nil {
			v.Projected.DefaultMode = ptr.To(mode)
		}
		for i := range v.Projected.Sources {
			src := &v.Projected.Sources[i]
			if src.DownwardAPI != nil {
				for j := range src.DownwardAPI.Items {
					defaultFieldRef(src.DownwardAPI.Items[j].FieldRef)
				}
			}
			if src.ServiceAccountToken != nil && src.ServiceAccountToken.ExpirationSeconds == nil {
				src.ServiceAccountToken.ExpirationSeconds = ptr.To[int64](3600)
			}
		}
	}
	if v.HostPath != nil && v.HostPath.Type == nil {
		v.HostPath.Type = ptr.To(corev1.HostPathUnset)
	}
	if v.Ephemeral != nil && v.Ephemeral.VolumeClaimTemplate != nil {
		defaultPVCSpec(&v.Ephemeral.VolumeClaimTemplate.Spec)
	}
}

func volumeSourceEmpty(vs *corev1.VolumeSource) bool {
	rv := reflect.ValueOf(vs).Elem()
	for i := range rv.NumField() {
		f := rv.Field(i)
		if f.Kind() == reflect.Pointer && !f.IsNil() {
			return false
		}
	}
	return true
}

func defaultPVC(pvc *corev1.PersistentVolumeClaim) {
	if pvc.Status.Phase == "" {
		pvc.Status.Phase = corev1.ClaimPending
	}
	defaultPVCSpec(&pvc.Spec)
}

func defaultPVCSpec(s *corev1.PersistentVolumeClaimSpec) {
	if s.VolumeMode == nil {
		s.VolumeMode = ptr.To(corev1.PersistentVolumeFilesystem)
	}
	roundResourceList(s.Resources.Limits)
	roundResourceList(s.Resources.Requests)
}

// roundResourceList rounds quantities up to milli precision, like the API server does.
func roundResourceList(rl corev1.ResourceList) {
	for k, v := range rl {
		v.RoundUp(resource.Milli)
		rl[k] = v
	}
}

func defaultReplicationController(rc *corev1.ReplicationController) {
	if rc.Spec.Template != nil {
		if labels := rc.Spec.Template.Labels; labels != nil {
			if len(rc.Spec.Selector) == 0 {
				rc.Spec.Selector = labels
			}
			if len(rc.Labels) == 0 {
				rc.Labels = labels
			}
		}
		defaultPodTemplateSpec(rc.Spec.Template)
	}
	if rc.Spec.Replicas == nil {
		rc.Spec.Replicas = ptr.To[int32](1)
	}
}

func defaultService(s *corev1.Service) {
	if s.Spec.SessionAffinity == "" {
		s.Spec.SessionAffinity = corev1.ServiceAffinityNone
	}
	if s.Spec.SessionAffinity == corev1.ServiceAffinityNone {
		s.Spec.SessionAffinityConfig = nil
	}
	if s.Spec.SessionAffinity == corev1.ServiceAffinityClientIP {
		cfg := s.Spec.SessionAffinityConfig
		if cfg == nil || cfg.ClientIP == nil || cfg.ClientIP.TimeoutSeconds == nil {
			s.Spec.SessionAffinityConfig = &corev1.SessionAffinityConfig{
				ClientIP: &corev1.ClientIPConfig{TimeoutSeconds: ptr.To(corev1.DefaultClientIPServiceAffinitySeconds)},
			}
		}
	}
	if s.Spec.Type == "" {
		s.Spec.Type = corev1.ServiceTypeClusterIP
	}
	for i := range s.Spec.Ports {
		p := &s.Spec.Ports[i]
		if p.Protocol == "" {
			p.Protocol = corev1.ProtocolTCP
		}
		if p.TargetPort == intstr.FromInt32(0) || p.TargetPort == intstr.FromString("") {
			p.TargetPort = intstr.FromInt32(p.Port)
		}
	}
	t := s.Spec.Type
	if (t == corev1.ServiceTypeNodePort || t == corev1.ServiceTypeLoadBalancer) && s.Spec.ExternalTrafficPolicy == "" {
		s.Spec.ExternalTrafficPolicy = corev1.ServiceExternalTrafficPolicyCluster
	}
	if s.Spec.InternalTrafficPolicy == nil && (t == corev1.ServiceTypeNodePort || t == corev1.ServiceTypeLoadBalancer || t == corev1.ServiceTypeClusterIP) {
		s.Spec.InternalTrafficPolicy = ptr.To(corev1.ServiceInternalTrafficPolicyCluster)
	}
	if t == corev1.ServiceTypeLoadBalancer && s.Spec.AllocateLoadBalancerNodePorts == nil {
		s.Spec.AllocateLoadBalancerNodePorts = ptr.To(true)
	}
}

func defaultDeployment(d *appsv1.Deployment) {
	if d.Spec.Replicas == nil {
		d.Spec.Replicas = ptr.To[int32](1)
	}
	st := &d.Spec.Strategy
	if st.Type == "" {
		st.Type = appsv1.RollingUpdateDeploymentStrategyType
	}
	if st.Type == appsv1.RollingUpdateDeploymentStrategyType {
		if st.RollingUpdate == nil {
			st.RollingUpdate = &appsv1.RollingUpdateDeployment{}
		}
		if st.RollingUpdate.MaxUnavailable == nil {
			st.RollingUpdate.MaxUnavailable = ptr.To(intstr.FromString("25%"))
		}
		if st.RollingUpdate.MaxSurge == nil {
			st.RollingUpdate.MaxSurge = ptr.To(intstr.FromString("25%"))
		}
	}
	if d.Spec.RevisionHistoryLimit == nil {
		d.Spec.RevisionHistoryLimit = ptr.To[int32](10)
	}
	if d.Spec.ProgressDeadlineSeconds == nil {
		d.Spec.ProgressDeadlineSeconds = ptr.To[int32](600)
	}
	defaultPodTemplateSpec(&d.Spec.Template)
}

func defaultStatefulSet(s *appsv1.StatefulSet) {
	if s.Spec.PodManagementPolicy == "" {
		s.Spec.PodManagementPolicy = appsv1.OrderedReadyPodManagement
	}
	if s.Spec.UpdateStrategy.Type == "" {
		s.Spec.UpdateStrategy.Type = appsv1.RollingUpdateStatefulSetStrategyType
		if s.Spec.UpdateStrategy.RollingUpdate == nil {
			s.Spec.UpdateStrategy.RollingUpdate = &appsv1.RollingUpdateStatefulSetStrategy{}
		}
	}
	if s.Spec.UpdateStrategy.Type == appsv1.RollingUpdateStatefulSetStrategyType &&
		s.Spec.UpdateStrategy.RollingUpdate != nil && s.Spec.UpdateStrategy.RollingUpdate.Partition == nil {
		s.Spec.UpdateStrategy.RollingUpdate.Partition = ptr.To[int32](0)
	}
	if s.Spec.PersistentVolumeClaimRetentionPolicy == nil {
		s.Spec.PersistentVolumeClaimRetentionPolicy = &appsv1.StatefulSetPersistentVolumeClaimRetentionPolicy{}
	}
	if s.Spec.PersistentVolumeClaimRetentionPolicy.WhenDeleted == "" {
		s.Spec.PersistentVolumeClaimRetentionPolicy.WhenDeleted = appsv1.RetainPersistentVolumeClaimRetentionPolicyType
	}
	if s.Spec.PersistentVolumeClaimRetentionPolicy.WhenScaled == "" {
		s.Spec.PersistentVolumeClaimRetentionPolicy.WhenScaled = appsv1.RetainPersistentVolumeClaimRetentionPolicyType
	}
	if s.Spec.Replicas == nil {
		s.Spec.Replicas = ptr.To[int32](1)
	}
	if s.Spec.RevisionHistoryLimit == nil {
		s.Spec.RevisionHistoryLimit = ptr.To[int32](10)
	}
	defaultPodTemplateSpec(&s.Spec.Template)
	for i := range s.Spec.VolumeClaimTemplates {
		defaultPVC(&s.Spec.VolumeClaimTemplates[i])
	}
}

func defaultDaemonSet(d *appsv1.DaemonSet) {
	us := &d.Spec.UpdateStrategy
	if us.Type == "" {
		us.Type = appsv1.RollingUpdateDaemonSetStrategyType
	}
	if us.Type == appsv1.RollingUpdateDaemonSetStrategyType {
		if us.RollingUpdate == nil {
			us.RollingUpdate = &appsv1.RollingUpdateDaemonSet{}
		}
		if us.RollingUpdate.MaxUnavailable == nil {
			us.RollingUpdate.MaxUnavailable = ptr.To(intstr.FromInt32(1))
		}
		if us.RollingUpdate.MaxSurge == nil {
			us.RollingUpdate.MaxSurge = ptr.To(intstr.FromInt32(0))
		}
	}
	if d.Spec.RevisionHistoryLimit == nil {
		d.Spec.RevisionHistoryLimit = ptr.To[int32](10)
	}
	defaultPodTemplateSpec(&d.Spec.Template)
}

func defaultJob(j *batchv1.Job) {
	if j.Spec.Completions == nil && j.Spec.Parallelism == nil {
		j.Spec.Completions = ptr.To[int32](1)
		j.Spec.Parallelism = ptr.To[int32](1)
	}
	if j.Spec.Parallelism == nil {
		j.Spec.Parallelism = ptr.To[int32](1)
	}
	if j.Spec.BackoffLimit == nil {
		if j.Spec.BackoffLimitPerIndex != nil {
			j.Spec.BackoffLimit = ptr.To[int32](2147483647)
		} else {
			j.Spec.BackoffLimit = ptr.To[int32](6)
		}
	}
	if labels := j.Spec.Template.Labels; labels != nil && len(j.Labels) == 0 {
		j.Labels = labels
	}
	if j.Spec.CompletionMode == nil {
		j.Spec.CompletionMode = ptr.To(batchv1.NonIndexedCompletion)
	}
	if j.Spec.Suspend == nil {
		j.Spec.Suspend = ptr.To(false)
	}
	if j.Spec.PodFailurePolicy != nil {
		for i := range j.Spec.PodFailurePolicy.Rules {
			for k := range j.Spec.PodFailurePolicy.Rules[i].OnPodConditions {
				c := &j.Spec.PodFailurePolicy.Rules[i].OnPodConditions[k]
				if c.Status == "" {
					c.Status = corev1.ConditionTrue
				}
			}
		}
	}
	if j.Spec.PodReplacementPolicy == nil {
		if j.Spec.PodFailurePolicy != nil {
			j.Spec.PodReplacementPolicy = ptr.To(batchv1.Failed)
		} else {
			j.Spec.PodReplacementPolicy = ptr.To(batchv1.TerminatingOrFailed)
		}
	}
	defaultPodTemplateSpec(&j.Spec.Template)
}

func defaultCronJob(c *batchv1.CronJob) {
	if c.Spec.ConcurrencyPolicy == "" {
		c.Spec.ConcurrencyPolicy = batchv1.AllowConcurrent
	}
	if c.Spec.Suspend == nil {
		c.Spec.Suspend = ptr.To(false)
	}
	if c.Spec.SuccessfulJobsHistoryLimit == nil {
		c.Spec.SuccessfulJobsHistoryLimit = ptr.To[int32](3)
	}
	if c.Spec.FailedJobsHistoryLimit == nil {
		c.Spec.FailedJobsHistoryLimit = ptr.To[int32](1)
	}
	defaultPodTemplateSpec(&c.Spec.JobTemplate.Spec.Template)
}

func defaultNetworkPolicy(np *networkingv1.NetworkPolicy) {
	for i := range np.Spec.Ingress {
		for j := range np.Spec.Ingress[i].Ports {
			if np.Spec.Ingress[i].Ports[j].Protocol == nil {
				np.Spec.Ingress[i].Ports[j].Protocol = ptr.To(corev1.ProtocolTCP)
			}
		}
	}
	for i := range np.Spec.Egress {
		for j := range np.Spec.Egress[i].Ports {
			if np.Spec.Egress[i].Ports[j].Protocol == nil {
				np.Spec.Egress[i].Ports[j].Protocol = ptr.To(corev1.ProtocolTCP)
			}
		}
	}
	if len(np.Spec.PolicyTypes) == 0 {
		np.Spec.PolicyTypes = []networkingv1.PolicyType{networkingv1.PolicyTypeIngress}
		if len(np.Spec.Egress) != 0 {
			np.Spec.PolicyTypes = append(np.Spec.PolicyTypes, networkingv1.PolicyTypeEgress)
		}
	}
}

func defaultHPAv2(h *autoscalingv2.HorizontalPodAutoscaler) {
	if h.Spec.MinReplicas == nil {
		h.Spec.MinReplicas = ptr.To[int32](1)
	}
	if len(h.Spec.Metrics) == 0 {
		h.Spec.Metrics = []autoscalingv2.MetricSpec{{
			Type: autoscalingv2.ResourceMetricSourceType,
			Resource: &autoscalingv2.ResourceMetricSource{
				Name: corev1.ResourceCPU,
				Target: autoscalingv2.MetricTarget{
					Type:               autoscalingv2.UtilizationMetricType,
					AverageUtilization: ptr.To[int32](80),
				},
			},
		}}
	}
	if b := h.Spec.Behavior; b != nil {
		b.ScaleUp = mergeScalingRules(b.ScaleUp, &autoscalingv2.HPAScalingRules{
			StabilizationWindowSeconds: ptr.To[int32](0),
			SelectPolicy:               ptr.To(autoscalingv2.MaxChangePolicySelect),
			Policies: []autoscalingv2.HPAScalingPolicy{
				{Type: autoscalingv2.PodsScalingPolicy, Value: 4, PeriodSeconds: 15},
				{Type: autoscalingv2.PercentScalingPolicy, Value: 100, PeriodSeconds: 15},
			},
		})
		b.ScaleDown = mergeScalingRules(b.ScaleDown, &autoscalingv2.HPAScalingRules{
			SelectPolicy: ptr.To(autoscalingv2.MaxChangePolicySelect),
			Policies: []autoscalingv2.HPAScalingPolicy{
				{Type: autoscalingv2.PercentScalingPolicy, Value: 100, PeriodSeconds: 15},
			},
		})
	}
}

func mergeScalingRules(given, defaults *autoscalingv2.HPAScalingRules) *autoscalingv2.HPAScalingRules {
	if given == nil {
		return defaults
	}
	if given.StabilizationWindowSeconds != nil {
		defaults.StabilizationWindowSeconds = given.StabilizationWindowSeconds
	}
	if given.SelectPolicy != nil {
		defaults.SelectPolicy = given.SelectPolicy
	}
	if given.Policies != nil {
		defaults.Policies = given.Policies
	}
	if given.Tolerance != nil {
		defaults.Tolerance = given.Tolerance
	}
	return defaults
}

func defaultWebhookCommon(
	failure **admissionregistrationv1.FailurePolicyType,
	match **admissionregistrationv1.MatchPolicyType,
	nsSel, objSel **metav1.LabelSelector,
	timeout **int32,
	rules []admissionregistrationv1.RuleWithOperations,
	cc *admissionregistrationv1.WebhookClientConfig,
) {
	if *failure == nil {
		*failure = ptr.To(admissionregistrationv1.Fail)
	}
	if *match == nil {
		*match = ptr.To(admissionregistrationv1.Equivalent)
	}
	if *nsSel == nil {
		*nsSel = &metav1.LabelSelector{}
	}
	if *objSel == nil {
		*objSel = &metav1.LabelSelector{}
	}
	if *timeout == nil {
		*timeout = ptr.To[int32](10)
	}
	for i := range rules {
		if rules[i].Scope == nil {
			rules[i].Scope = ptr.To(admissionregistrationv1.AllScopes)
		}
	}
	if cc.Service != nil && cc.Service.Port == nil {
		cc.Service.Port = ptr.To[int32](443)
	}
}

// defaultWebhookV1beta1 applies the admissionregistration/v1beta1 webhook defaults, which
// differ from v1 (failurePolicy Ignore, matchPolicy Exact, sideEffects Unknown, timeout 30s,
// admissionReviewVersions [v1beta1]).
func defaultWebhookV1beta1(
	failure **admissionregistrationv1beta1.FailurePolicyType,
	match **admissionregistrationv1beta1.MatchPolicyType,
	nsSel, objSel **metav1.LabelSelector,
	sideEffects **admissionregistrationv1beta1.SideEffectClass,
	timeout **int32,
	reviewVersions *[]string,
	rules []admissionregistrationv1beta1.RuleWithOperations,
	cc *admissionregistrationv1beta1.WebhookClientConfig,
) {
	if *failure == nil {
		*failure = ptr.To(admissionregistrationv1beta1.Ignore)
	}
	if *match == nil {
		*match = ptr.To(admissionregistrationv1beta1.Exact)
	}
	if *nsSel == nil {
		*nsSel = &metav1.LabelSelector{}
	}
	if *objSel == nil {
		*objSel = &metav1.LabelSelector{}
	}
	if *sideEffects == nil {
		*sideEffects = ptr.To(admissionregistrationv1beta1.SideEffectClassUnknown)
	}
	if *timeout == nil {
		*timeout = ptr.To[int32](30)
	}
	if len(*reviewVersions) == 0 {
		*reviewVersions = []string{"v1beta1"}
	}
	for i := range rules {
		if rules[i].Scope == nil {
			rules[i].Scope = ptr.To(admissionregistrationv1beta1.AllScopes)
		}
	}
	if cc.Service != nil && cc.Service.Port == nil {
		cc.Service.Port = ptr.To[int32](443)
	}
}
