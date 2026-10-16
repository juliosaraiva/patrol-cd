package lua

import (
	"errors"

	"github.com/prometheus/client_golang/prometheus"
)

// RegisterMetrics registers the Lua runtime's watchdog metrics with reg:
//
//	patrol_lua_stalled_evaluations          abandoned evaluations still running
//	patrol_lua_abandoned_evaluations_total  evaluations abandoned since process start
//
// An evaluation is abandoned when it is still running stallGrace after its deadline,
// which means a Go library call ignored the deadline. A non-zero gauge points at a
// customization that needs attention; at maxStalled, evaluations fail with
// ErrOverloaded. Registering twice with the same registry is not an error.
func RegisterMetrics(reg prometheus.Registerer) error {
	collectors := []prometheus.Collector{
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "patrol_lua_stalled_evaluations",
			Help: "Lua evaluations abandoned past their deadline that are still running.",
		}, func() float64 { return float64(stalledEvaluations.Load()) }),
		prometheus.NewCounterFunc(prometheus.CounterOpts{
			Name: "patrol_lua_abandoned_evaluations_total",
			Help: "Lua evaluations abandoned because they ran past their deadline.",
		}, func() float64 { return float64(abandonedEvaluations.Load()) }),
	}
	for _, c := range collectors {
		if err := reg.Register(c); err != nil {
			var are prometheus.AlreadyRegisteredError
			if errors.As(err, &are) {
				continue
			}
			return err
		}
	}
	return nil
}
