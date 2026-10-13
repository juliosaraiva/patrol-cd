package health

import "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

// AnnotationIgnoreHealthCheck excludes a live resource from application health when set to
// exactly "true". It must stay equal to api/v1alpha1.AnnotationKeyIgnoreHealthCheck.
const AnnotationIgnoreHealthCheck = "cd.patrol.example/ignore-healthcheck"

// IgnoresHealthCheck reports whether live carries [AnnotationIgnoreHealthCheck] with the
// exact value "true" (other spellings such as "True" are not recognized, as upstream).
func IgnoresHealthCheck(live *unstructured.Unstructured) bool {
	return live != nil && live.GetAnnotations()[AnnotationIgnoreHealthCheck] == "true"
}

// ResourceHealth is one managed resource of an application, as input to [Aggregate].
//
// Callers classify hooks and annotations (that logic belongs to the sync and tracking
// packages) and assess live objects with [Assess]; Aggregate applies the application rule.
type ResourceHealth struct {
	// Group and Kind identify the resource type (Group is "" for the core group).
	Group string
	Kind  string
	// Live reports whether the resource exists in the cluster.
	Live bool
	// Hook excludes the resource entirely: its target is a hook of type Skip only, or its
	// live object is a sync hook, or its live object is a hook of an unknown type (ignored).
	Hook bool
	// IgnoreHealthCheck is set when the live object carries the ignore-healthcheck
	// annotation with value "true". The resource still counts as a live resource but its
	// health is neither recorded nor aggregated. Ignored when Live is false.
	IgnoreHealthCheck bool
	// SelfReference is set when the live object is the Application being assessed (an app
	// that manages itself). Same effect as IgnoreHealthCheck. Ignored when Live is false.
	SelfReference bool
	// Health is the assessed health of the live object; nil when the kind has no health.
	// Ignored when Live is false (the resource is Missing).
	Health *HealthStatus
}

// AggregateResult is the outcome of [AggregateDetailed].
type AggregateResult struct {
	// Status is the application health.
	Status Status
	// Resources holds, per input index, the health to record for that resource in the
	// application's resource list (nil when nothing is recorded). Callers that do not
	// persist resource health inline simply ignore it.
	Resources []*HealthStatus
}

// Aggregate returns application health from its managed resources. See
// [AggregateDetailed] for the rule.
func Aggregate(resources []ResourceHealth) Status {
	return AggregateDetailed(resources).Status
}

// AggregateDetailed applies the application health rule (upstream controller/health.go):
//
//   - Start Healthy; the application takes the worst ([IsWorse]) contributing health.
//   - Hooks are excluded entirely.
//   - A resource without live state is recorded as Missing but does not contribute (the
//     OutOfSync status already reports it).
//   - IgnoreHealthCheck and SelfReference resources count as present but neither record nor
//     contribute health; resources whose Health is nil (kinds without health) neither.
//   - A child Patrol Application whose health is Missing or Unknown does not contribute.
//   - If the application has non-hook resources but none of them is live, its health is
//     Missing (unless something already made it worse, which cannot happen here).
func AggregateDetailed(resources []ResourceHealth) AggregateResult {
	res := AggregateResult{Status: StatusHealthy, Resources: make([]*HealthStatus, len(resources))}
	var hasResources, hasLive bool
	for i := range resources {
		r := &resources[i]
		if r.Hook {
			continue
		}
		hasResources = true
		if r.Live {
			hasLive = true
			if r.IgnoreHealthCheck || r.SelfReference {
				continue
			}
		}

		var hs *HealthStatus
		if !r.Live {
			hs = &HealthStatus{Status: StatusMissing}
		} else if r.Health != nil {
			c := *r.Health
			hs = &c
		}
		if hs == nil {
			continue
		}
		res.Resources[i] = hs

		if !r.Live {
			continue
		}
		if r.Group == ApplicationGroup && r.Kind == ApplicationKind &&
			(hs.Status == StatusMissing || hs.Status == StatusUnknown) {
			continue
		}
		if IsWorse(res.Status, hs.Status) {
			res.Status = hs.Status
		}
	}
	if hasResources && !hasLive && IsWorse(res.Status, StatusMissing) {
		res.Status = StatusMissing
	}
	return res
}
