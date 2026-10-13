package services

import (
	"fmt"
	"sort"
)

// Service type names of the baseline (the <type> of service.<type>[.<name>] keys).
const (
	TypeAlertmanager   = "alertmanager"
	TypeAwsSqs         = "awssqs"
	TypeEmail          = "email"
	TypeGCPPubSub      = "gcppubsub"
	TypeGitHub         = "github"
	TypeGoogleChat     = "googlechat"
	TypeGrafana        = "grafana"
	TypeMattermost     = "mattermost"
	TypeNats           = "nats"
	TypeNewRelic       = "newrelic"
	TypeOpsgenie       = "opsgenie"
	TypePagerDuty      = "pagerduty"
	TypePagerDutyV2    = "pagerdutyv2"
	TypePushover       = "pushover"
	TypeRocketChat     = "rocketchat"
	TypeSlack          = "slack"
	TypeTeams          = "teams"
	TypeTeamsWorkflows = "teams-workflows"
	TypeTelegram       = "telegram"
	TypeWebex          = "webex"
	TypeWebhook        = "webhook"
)

type constructor func(raw []byte, deps Deps, r *Redactor) (Service, error)

var constructors = map[string]constructor{
	TypeAlertmanager:   newAlertmanagerService,
	TypeAwsSqs:         newAwsSqsService,
	TypeEmail:          newEmailService,
	TypeGCPPubSub:      newGCPPubSubService,
	TypeGitHub:         newGitHubService,
	TypeGoogleChat:     newGoogleChatService,
	TypeGrafana:        newGrafanaService,
	TypeMattermost:     newMattermostService,
	TypeNats:           newNatsService,
	TypeNewRelic:       newNewRelicService,
	TypeOpsgenie:       newOpsgenieService,
	TypePagerDuty:      newPagerDutyService,
	TypePagerDutyV2:    newPagerDutyV2Service,
	TypePushover:       newPushoverService,
	TypeRocketChat:     newRocketChatService,
	TypeSlack:          newSlackService,
	TypeTeams:          newTeamsService,
	TypeTeamsWorkflows: newTeamsWorkflowsService,
	TypeTelegram:       newTelegramService,
	TypeWebex:          newWebexService,
	TypeWebhook:        newWebhookService,
}

// SupportedTypes returns the sorted list of supported service types.
func SupportedTypes() []string {
	out := make([]string, 0, len(constructors))
	for k := range constructors {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// IsSupportedType reports whether serviceType is a known service type.
func IsSupportedType(serviceType string) bool {
	_, ok := constructors[serviceType]
	return ok
}

// New constructs a service of the given type from JSON options (already secret
// substituted). The returned service redacts deps.SensitiveValues and the service's own
// credential fields from every error. Construction errors are redacted too.
func New(serviceType string, rawOptions []byte, deps Deps) (Service, error) {
	ctor, ok := constructors[serviceType]
	if !ok {
		return nil, fmt.Errorf("%w: service type '%s' is not supported", ErrUnsupportedServiceType, serviceType)
	}
	redactor := NewRedactor(deps.SensitiveValues...)
	svc, err := ctor(rawOptions, deps, redactor)
	if err != nil {
		return nil, &redactedError{msg: redactor.Redact(err.Error()), cause: err}
	}
	return &redactingService{typ: serviceType, inner: svc, redactor: redactor}, nil
}
