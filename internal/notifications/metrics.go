package notifications

import (
	"errors"
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
)

// Metrics receives engine instrumentation events. Implementations must be safe for
// concurrent use.
type Metrics interface {
	// IncDelivery counts a delivery attempt outcome.
	IncDelivery(trigger, service string, succeeded bool)
	// IncTriggerEvaluation counts a trigger condition evaluation.
	IncTriggerEvaluation(trigger string, triggered bool)
}

type noopMetrics struct{}

func (noopMetrics) IncDelivery(string, string, bool)  {}
func (noopMetrics) IncTriggerEvaluation(string, bool) {}

// PrometheusMetrics exports the baseline notification metrics with the patrol_ prefix:
//
//	patrol_notifications_deliveries_total{trigger, service, succeeded}
//	patrol_notifications_trigger_eval_total{name, triggered}
type PrometheusMetrics struct {
	deliveries  *prometheus.CounterVec
	evaluations *prometheus.CounterVec
}

// NewPrometheusMetrics creates and registers the notification metrics. An already
// registered identical collector is reused.
func NewPrometheusMetrics(reg prometheus.Registerer) (*PrometheusMetrics, error) {
	m := &PrometheusMetrics{
		deliveries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "patrol_notifications_deliveries_total",
			Help: "Number of notification delivery attempts.",
		}, []string{"trigger", "service", "succeeded"}),
		evaluations: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "patrol_notifications_trigger_eval_total",
			Help: "Number of notification trigger condition evaluations.",
		}, []string{"name", "triggered"}),
	}
	if reg == nil {
		return m, nil
	}
	var err error
	if m.deliveries, err = registerCounterVec(reg, m.deliveries); err != nil {
		return nil, err
	}
	if m.evaluations, err = registerCounterVec(reg, m.evaluations); err != nil {
		return nil, err
	}
	return m, nil
}

func registerCounterVec(reg prometheus.Registerer, c *prometheus.CounterVec) (*prometheus.CounterVec, error) {
	if err := reg.Register(c); err != nil {
		var are prometheus.AlreadyRegisteredError
		if errors.As(err, &are) {
			if existing, ok := are.ExistingCollector.(*prometheus.CounterVec); ok {
				return existing, nil
			}
		}
		return nil, err
	}
	return c, nil
}

// IncDelivery implements Metrics.
func (m *PrometheusMetrics) IncDelivery(trigger, service string, succeeded bool) {
	m.deliveries.WithLabelValues(trigger, service, strconv.FormatBool(succeeded)).Inc()
}

// IncTriggerEvaluation implements Metrics.
func (m *PrometheusMetrics) IncTriggerEvaluation(trigger string, triggered bool) {
	m.evaluations.WithLabelValues(trigger, strconv.FormatBool(triggered)).Inc()
}
