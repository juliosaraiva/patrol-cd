package appset

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

func rollingSync(steps ...v1alpha1.ApplicationSetRolloutStep) *v1alpha1.ApplicationSetStrategy {
	return &v1alpha1.ApplicationSetStrategy{
		Type:        v1alpha1.ApplicationSetStrategyRollingSync,
		RollingSync: &v1alpha1.ApplicationSetRolloutStrategy{Steps: steps},
	}
}

func TestConditionsSuccessPath(t *testing.T) {
	t.Parallel()
	as := &v1alpha1.ApplicationSet{}
	t0 := metav1.NewTime(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	conds, eval := conditionsFor(as, condition{
		Type: v1alpha1.ApplicationSetConditionResourcesUpToDate, Status: metav1.ConditionTrue,
		Reason: v1alpha1.ApplicationSetReasonApplicationSetUpToDate, Message: msgUpToDate,
	}, true)
	applyConditions(&as.Status, conds, eval, 3, t0)
	assert.Equal(t, []string{
		"ErrorOccurred|False|ApplicationSetUpToDate|All applications have been generated successfully",
		"ParametersGenerated|True|ParametersGenerated|Successfully generated parameters for all Applications",
		"ResourcesUpToDate|True|ApplicationSetUpToDate|All applications have been generated successfully",
	}, conditionSummary(as))
	assert.Equal(t, v1alpha1.HealthStatus{Status: v1alpha1.HealthStatusHealthy, Message: msgUpToDate}, as.Status.Health)
	for _, c := range as.Status.Conditions {
		assert.EqualValues(t, 3, c.ObservedGeneration)
	}

	// Unchanged conditions keep their lastTransitionTime.
	t1 := metav1.NewTime(t0.Add(time.Hour))
	applyConditions(&as.Status, conds, eval, 3, t1)
	for _, c := range as.Status.Conditions {
		assert.True(t, c.LastTransitionTime.Equal(&t0), c.Type)
	}
}

func TestConditionsGenerationFailure(t *testing.T) {
	t.Parallel()
	as := &v1alpha1.ApplicationSet{}
	conds, eval := conditionsFor(as, condition{
		Type: v1alpha1.ApplicationSetConditionErrorOccurred, Status: metav1.ConditionTrue,
		Reason: v1alpha1.ApplicationSetReasonApplicationParamsGenerationError, Message: "boom",
	}, false)
	applyConditions(&as.Status, conds, eval, 1, metav1.Now())
	assert.Equal(t, []string{
		"ErrorOccurred|True|ApplicationGenerationFromParamsError|boom",
		"ParametersGenerated|False|ErrorOccurred|boom",
		"ResourcesUpToDate|False|ErrorOccurred|boom",
	}, conditionSummary(as))
	assert.Equal(t, v1alpha1.HealthStatus{Status: v1alpha1.HealthStatusDegraded, Message: "boom"}, as.Status.Health)
}

func TestRolloutConditionRemovedWithoutRollingSync(t *testing.T) {
	t.Parallel()
	as := &v1alpha1.ApplicationSet{Status: v1alpha1.ApplicationSetStatus{Conditions: []metav1.Condition{
		{Type: v1alpha1.ApplicationSetConditionRolloutProgressing, Status: metav1.ConditionTrue, Reason: "ApplicationSetModified"},
	}}}
	conds, eval := conditionsFor(as, condition{
		Type: v1alpha1.ApplicationSetConditionResourcesUpToDate, Status: metav1.ConditionTrue,
		Reason: v1alpha1.ApplicationSetReasonApplicationSetUpToDate, Message: msgUpToDate,
	}, true)
	applyConditions(&as.Status, conds, eval, 1, metav1.Now())
	assert.Nil(t, findCondition(as, v1alpha1.ApplicationSetConditionRolloutProgressing))

	// With RollingSync the condition is kept and drives health.
	as.Spec.Strategy = rollingSync(v1alpha1.ApplicationSetRolloutStep{})
	conds, eval = conditionsFor(as, condition{
		Type: v1alpha1.ApplicationSetConditionRolloutProgressing, Status: metav1.ConditionTrue,
		Reason: v1alpha1.ApplicationSetReasonApplicationSetModified, Message: msgRolloutStepPrefix + "1",
	}, true)
	applyConditions(&as.Status, conds, eval, 1, metav1.Now())
	assert.Equal(t, v1alpha1.HealthStatusProgressing, as.Status.Health.Status)
	assert.Equal(t, "ApplicationSet is performing rollout of step 1", as.Status.Health.Message)
}

func TestCalculateHealth(t *testing.T) {
	t.Parallel()
	cond := func(typ string, status metav1.ConditionStatus, msg string) metav1.Condition {
		return metav1.Condition{Type: typ, Status: status, Message: msg}
	}
	tests := []struct {
		name  string
		conds []metav1.Condition
		want  v1alpha1.HealthStatus
	}{
		{"none", nil, v1alpha1.HealthStatus{Status: v1alpha1.HealthStatusUnknown, Message: msgNoConditions}},
		{"error wins", []metav1.Condition{
			cond(v1alpha1.ApplicationSetConditionResourcesUpToDate, metav1.ConditionTrue, "ok"),
			cond(v1alpha1.ApplicationSetConditionRolloutProgressing, metav1.ConditionTrue, "rolling"),
			cond(v1alpha1.ApplicationSetConditionErrorOccurred, metav1.ConditionTrue, "bad"),
		}, v1alpha1.HealthStatus{Status: v1alpha1.HealthStatusDegraded, Message: "bad"}},
		{"progressing over healthy", []metav1.Condition{
			cond(v1alpha1.ApplicationSetConditionResourcesUpToDate, metav1.ConditionTrue, "ok"),
			cond(v1alpha1.ApplicationSetConditionRolloutProgressing, metav1.ConditionTrue, "rolling"),
		}, v1alpha1.HealthStatus{Status: v1alpha1.HealthStatusProgressing, Message: "rolling"}},
		{"healthy", []metav1.Condition{cond(v1alpha1.ApplicationSetConditionResourcesUpToDate, metav1.ConditionTrue, "ok")},
			v1alpha1.HealthStatus{Status: v1alpha1.HealthStatusHealthy, Message: "ok"}},
		{"undetermined", []metav1.Condition{cond(v1alpha1.ApplicationSetConditionParametersGenerated, metav1.ConditionTrue, "x")},
			v1alpha1.HealthStatus{Status: v1alpha1.HealthStatusUnknown, Message: msgWaitingForHealth}},
	}
	for _, tt := range tests {
		got := CalculateHealth(&v1alpha1.ApplicationSetStatus{Conditions: tt.conds})
		assert.Equal(t, tt.want, got, tt.name)
	}
}

func TestTruncateMessage(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", maxConditionMessage+10)
	got := truncateMessage(long)
	require.Len(t, got, maxConditionMessage)
	assert.True(t, strings.HasSuffix(got, "(truncated)"))
	assert.Equal(t, "short", truncateMessage("short"))
}
