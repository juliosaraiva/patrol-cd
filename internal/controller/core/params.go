package core

import (
	"time"

	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/juliosaraiva/patrol-cd/internal/settings"
)

// Controller parameters (patrol-cmd-params-cm keys, Argo CD argocd-cmd-params-cm names).
const (
	ParamApplicationNamespaces   = "application.namespaces"
	ParamStatusProcessors        = "controller.status.processors"
	ParamOperationProcessors     = "controller.operation.processors"
	ParamSelfHealTimeout         = "controller.self.heal.timeout.seconds"
	ParamSelfHealBackoffTimeout  = "controller.self.heal.backoff.timeout.seconds"
	ParamSelfHealBackoffFactor   = "controller.self.heal.backoff.factor"
	ParamSelfHealBackoffCap      = "controller.self.heal.backoff.cap.seconds"
	ParamSyncTimeout             = "controller.sync.timeout.seconds"
	ParamSyncWaveDelay           = "controller.sync.wave.delay.seconds"
	ParamResourceHealthPersist   = "controller.resource.health.persist"
	ParamRepoErrorGracePeriod    = "controller.repo.error.grace.period.seconds"
	ParamServerSideDiff          = "controller.diff.server.side"
	ParamKubectlParallelismLimit = "controller.kubectl.parallelism.limit"
)

// Params are the controller tuning knobs shared by both reconcilers. The refresh intervals
// (timeout.reconciliation, its jitter and timeout.hard.reconciliation) are patrol-cm settings
// and are read from the current settings snapshot on every reconcile instead.
type Params struct {
	// ApplicationNamespaces are the namespaces (globs or /regex/) besides the control-plane
	// namespace whose Applications are reconciled (application.namespaces).
	ApplicationNamespaces []string
	// StatusProcessors is the number of concurrent Application reconciles.
	StatusProcessors int
	// OperationProcessors is the number of concurrent Operation reconciles.
	OperationProcessors int
	// SelfHealTimeout is the fixed delay between self-heal attempts, used only when
	// SelfHealBackoff is nil (controller.self.heal.timeout.seconds).
	SelfHealTimeout time.Duration
	// SelfHealBackoff is the exponential self-heal backoff; nil when
	// controller.self.heal.backoff.timeout.seconds is 0.
	SelfHealBackoff *wait.Backoff
	// SyncTimeout terminates Running operations older than it (0 disables;
	// controller.sync.timeout.seconds).
	SyncTimeout time.Duration
}

// DefaultParams returns the upstream defaults: 20 status and 10 operation processors,
// self-heal backoff 2s × 3 capped at 300s, no sync timeout.
func DefaultParams() Params {
	return Params{
		StatusProcessors:    20,
		OperationProcessors: 10,
		SelfHealBackoff:     &wait.Backoff{Duration: 2 * time.Second, Factor: 3, Cap: 300 * time.Second},
	}
}

// ParamsFrom maps resolved patrol-cmd-params-cm parameters of the controller component.
// Keys that are absent keep the defaults of DefaultParams.
func ParamsFrom(p *settings.Params) Params {
	out := DefaultParams()
	if p == nil {
		return out
	}
	if _, ok := p.Lookup(ParamApplicationNamespaces); ok {
		out.ApplicationNamespaces = p.Strings(ParamApplicationNamespaces)
	}
	if _, ok := p.Lookup(ParamStatusProcessors); ok {
		out.StatusProcessors = int(p.Int(ParamStatusProcessors))
	}
	if _, ok := p.Lookup(ParamOperationProcessors); ok {
		out.OperationProcessors = int(p.Int(ParamOperationProcessors))
	}
	if _, ok := p.Lookup(ParamSelfHealTimeout); ok {
		out.SelfHealTimeout = time.Duration(p.Int(ParamSelfHealTimeout)) * time.Second
	}
	if _, ok := p.Lookup(ParamSelfHealBackoffTimeout); ok {
		base := time.Duration(p.Int(ParamSelfHealBackoffTimeout)) * time.Second
		if base == 0 {
			out.SelfHealBackoff = nil
		} else {
			out.SelfHealBackoff = &wait.Backoff{
				Duration: base,
				Factor:   float64(p.Int(ParamSelfHealBackoffFactor)),
				Cap:      time.Duration(p.Int(ParamSelfHealBackoffCap)) * time.Second,
			}
		}
	}
	if _, ok := p.Lookup(ParamSyncTimeout); ok {
		out.SyncTimeout = time.Duration(p.Int(ParamSyncTimeout)) * time.Second
	}
	if out.StatusProcessors <= 0 {
		out.StatusProcessors = 1
	}
	if out.OperationProcessors <= 0 {
		out.OperationProcessors = 1
	}
	return out
}
