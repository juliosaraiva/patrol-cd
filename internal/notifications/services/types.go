// Package services implements the delivery side of the Patrol notifications engine: the
// rendered Notification model (one optional section per service type), the Service
// interface and one implementation per built-in service type of the Argo CD v3.5.5
// notifications baseline.
//
// Every Service is safe for concurrent use. Errors returned by Send never contain
// configured secret values: each service is wrapped in a redacting decorator by New.
package services

import (
	"context"
	"errors"
	"fmt"
)

// Destination identifies where a notification is delivered: the configured service name
// (the <name> of a service.<type>[.<name>] ConfigMap key) and a service specific recipient
// (channel, e-mail address, room id, ...). Recipient may be empty for services that do not
// use it (webhook, github, alertmanager).
type Destination struct {
	Service   string `json:"service"`
	Recipient string `json:"recipient"`
}

// String renders the destination as "<service>:<recipient>".
func (d Destination) String() string { return d.Service + ":" + d.Recipient }

// Service delivers a rendered notification to a destination.
type Service interface {
	Send(ctx context.Context, n Notification, dest Destination) error
}

// Notification is both the template definition (every string field is a Go text/template)
// and, after rendering, the concrete notification handed to a Service. Field names and JSON
// keys follow the Argo CD notifications template format.
type Notification struct {
	Message        string                      `json:"message,omitempty"`
	Email          *EmailNotification          `json:"email,omitempty"`
	Slack          *SlackNotification          `json:"slack,omitempty"`
	Mattermost     *MattermostNotification     `json:"mattermost,omitempty"`
	RocketChat     *RocketChatNotification     `json:"rocketchat,omitempty"`
	Teams          *TeamsNotification          `json:"teams,omitempty"`
	TeamsWorkflows *TeamsWorkflowsNotification `json:"teams-workflows,omitempty"`
	GoogleChat     *GoogleChatNotification     `json:"googlechat,omitempty"`
	Webhook        WebhookNotifications        `json:"webhook,omitempty"`
	Opsgenie       *OpsgenieNotification       `json:"opsgenie,omitempty"`
	GitHub         *GitHubNotification         `json:"github,omitempty"`
	PagerDuty      *PagerDutyNotification      `json:"pagerduty,omitempty"`
	PagerDutyV2    *PagerDutyV2Notification    `json:"pagerdutyv2,omitempty"`
	NewRelic       *NewRelicNotification       `json:"newrelic,omitempty"`
	Alertmanager   *AlertmanagerNotification   `json:"alertmanager,omitempty"`
	AwsSqs         *AwsSqsNotification         `json:"awsSqs,omitempty"`
	GCPPubSub      *GCPPubSubNotification      `json:"gcppubsub,omitempty"`
}

// EmailNotification holds the e-mail specific template fields.
type EmailNotification struct {
	Subject string `json:"subject,omitempty"`
	Body    string `json:"body,omitempty"`
}

// Slack delivery policies (template field slack.deliveryPolicy).
const (
	SlackPolicyPost          = "Post"
	SlackPolicyPostAndUpdate = "PostAndUpdate"
	SlackPolicyUpdate        = "Update"
)

// SlackNotification holds the Slack specific template fields. Attachments and Blocks are
// JSON arrays rendered as strings.
type SlackNotification struct {
	Attachments     string `json:"attachments,omitempty"`
	Blocks          string `json:"blocks,omitempty"`
	GroupingKey     string `json:"groupingKey,omitempty"`
	NotifyBroadcast bool   `json:"notifyBroadcast,omitempty"`
	DeliveryPolicy  string `json:"deliveryPolicy,omitempty"`
	Username        string `json:"username,omitempty"`
	Icon            string `json:"icon,omitempty"`
}

// MattermostNotification holds the Mattermost specific template fields.
type MattermostNotification struct {
	Attachments string `json:"attachments,omitempty"`
}

// RocketChatNotification holds the Rocket.Chat specific template fields.
type RocketChatNotification struct {
	Attachments string `json:"attachments,omitempty"`
}

// TeamsNotification holds the Microsoft Teams (Office 365 connector, MessageCard) fields.
// Facts, Sections and PotentialAction are JSON arrays rendered as strings; Template is a
// complete raw JSON payload that replaces everything else.
type TeamsNotification struct {
	Template        string `json:"template,omitempty"`
	Title           string `json:"title,omitempty"`
	Summary         string `json:"summary,omitempty"`
	Text            string `json:"text,omitempty"`
	ThemeColor      string `json:"themeColor,omitempty"`
	Facts           string `json:"facts,omitempty"`
	Sections        string `json:"sections,omitempty"`
	PotentialAction string `json:"potentialAction,omitempty"`
}

// TeamsWorkflowsNotification holds the Teams Workflows (Power Automate, Adaptive Card)
// fields. AdaptiveCard, when set, takes precedence over every other field except Template.
type TeamsWorkflowsNotification struct {
	Template        string `json:"template,omitempty"`
	Title           string `json:"title,omitempty"`
	Summary         string `json:"summary,omitempty"`
	Text            string `json:"text,omitempty"`
	ThemeColor      string `json:"themeColor,omitempty"`
	Facts           string `json:"facts,omitempty"`
	Sections        string `json:"sections,omitempty"`
	PotentialAction string `json:"potentialAction,omitempty"`
	AdaptiveCard    string `json:"adaptiveCard,omitempty"`
}

// GoogleChatNotification holds the Google Chat fields. Cards and CardsV2 are YAML or JSON
// lists rendered as strings.
type GoogleChatNotification struct {
	Cards     string `json:"cards,omitempty"`
	CardsV2   string `json:"cardsV2,omitempty"`
	ThreadKey string `json:"threadKey,omitempty"`
}

// WebhookNotification customizes the request of one webhook service.
type WebhookNotification struct {
	Method string `json:"method,omitempty"`
	Path   string `json:"path,omitempty"`
	Body   string `json:"body,omitempty"`
}

// WebhookNotifications maps a webhook service name to its request customization.
type WebhookNotifications map[string]WebhookNotification

// OpsgenieResponder is a team or user an Opsgenie alert is made visible to.
type OpsgenieResponder struct {
	Type     string `json:"type,omitempty"`
	ID       string `json:"id,omitempty"`
	Name     string `json:"name,omitempty"`
	Username string `json:"username,omitempty"`
}

// OpsgenieNotification holds the Opsgenie alert fields.
type OpsgenieNotification struct {
	Description string              `json:"description,omitempty"`
	Priority    string              `json:"priority,omitempty"`
	Alias       string              `json:"alias,omitempty"`
	Note        string              `json:"note,omitempty"`
	Actions     []string            `json:"actions,omitempty"`
	Tags        []string            `json:"tags,omitempty"`
	VisibleTo   []OpsgenieResponder `json:"visibleTo,omitempty"`
	Details     map[string]string   `json:"details,omitempty"`
	Entity      string              `json:"entity,omitempty"`
	User        string              `json:"user,omitempty"`
}

// GitHubNotification holds the GitHub fields. RepoURLPath and RevisionPath are templates
// that render to the repository URL and the commit SHA the notification refers to.
type GitHubNotification struct {
	RepoURLPath        string                    `json:"repoURLPath,omitempty"`
	RevisionPath       string                    `json:"revisionPath,omitempty"`
	Status             *GitHubStatus             `json:"status,omitempty"`
	Deployment         *GitHubDeployment         `json:"deployment,omitempty"`
	PullRequestComment *GitHubPullRequestComment `json:"pullRequestComment,omitempty"`
	CheckRun           *GitHubCheckRun           `json:"checkRun,omitempty"`
}

// GitHubStatus is a commit status.
type GitHubStatus struct {
	State     string `json:"state,omitempty"`
	Label     string `json:"label,omitempty"`
	TargetURL string `json:"targetURL,omitempty"`
}

// GitHubDeployment is a deployment plus deployment status.
type GitHubDeployment struct {
	State                string   `json:"state,omitempty"`
	Environment          string   `json:"environment,omitempty"`
	EnvironmentURL       string   `json:"environmentURL,omitempty"`
	LogURL               string   `json:"logURL,omitempty"`
	RequiredContexts     []string `json:"requiredContexts"`
	AutoMerge            *bool    `json:"autoMerge,omitempty"`
	TransientEnvironment *bool    `json:"transientEnvironment,omitempty"`
	Reference            string   `json:"reference,omitempty"`
}

// GitHubPullRequestComment is a comment upserted on every pull request containing the
// revision. CommentTag identifies the comment to update.
type GitHubPullRequestComment struct {
	Content    string `json:"content,omitempty"`
	CommentTag string `json:"commentTag,omitempty"`
}

// GitHubCheckRun is a check run on the revision.
type GitHubCheckRun struct {
	Name        string                `json:"name,omitempty"`
	DetailsURL  string                `json:"details_url,omitempty"`
	Status      string                `json:"status,omitempty"`
	Conclusion  string                `json:"conclusion,omitempty"`
	StartedAt   string                `json:"started_at,omitempty"`
	CompletedAt string                `json:"completed_at,omitempty"`
	ExternalID  string                `json:"external_id,omitempty"`
	Output      *GitHubCheckRunOutput `json:"output,omitempty"`
}

// GitHubCheckRunOutput is the output section of a check run.
type GitHubCheckRunOutput struct {
	Title   string `json:"title,omitempty"`
	Summary string `json:"summary,omitempty"`
	Text    string `json:"text,omitempty"`
}

// PagerDutyNotification holds the PagerDuty (REST API incidents) fields.
type PagerDutyNotification struct {
	Title      string `json:"title,omitempty"`
	Body       string `json:"body,omitempty"`
	Urgency    string `json:"urgency,omitempty"`
	PriorityID string `json:"priorityID,omitempty"`
}

// PagerDutyV2Notification holds the PagerDuty Events API v2 fields.
type PagerDutyV2Notification struct {
	Summary   string `json:"summary,omitempty"`
	Severity  string `json:"severity,omitempty"`
	Source    string `json:"source,omitempty"`
	Component string `json:"component,omitempty"`
	Group     string `json:"group,omitempty"`
	Class     string `json:"class,omitempty"`
	URL       string `json:"url,omitempty"`
	DedupKey  string `json:"dedupKey,omitempty"`
}

// NewRelicNotification holds the New Relic deployment marker fields.
type NewRelicNotification struct {
	Revision    string `json:"revision,omitempty"`
	Changelog   string `json:"changelog,omitempty"`
	Description string `json:"description,omitempty"`
	User        string `json:"user,omitempty"`
}

// AlertmanagerNotification holds the Alertmanager alert fields.
type AlertmanagerNotification struct {
	Labels       map[string]string `json:"labels,omitempty"`
	Annotations  map[string]string `json:"annotations,omitempty"`
	GeneratorURL string            `json:"generatorURL,omitempty"`
}

// AwsSqsNotification holds the AWS SQS message fields.
type AwsSqsNotification struct {
	MessageAttributes      map[string]string `json:"messageAttributes,omitempty"`
	MessageGroupID         string            `json:"messageGroupId,omitempty"`
	MessageDeduplicationID string            `json:"messageDeduplicationId,omitempty"`
}

// GCPPubSubNotification holds the Google Cloud Pub/Sub message fields.
type GCPPubSubNotification struct {
	Attributes map[string]string `json:"attributes,omitempty"`
}

// Sentinel errors. Errors returned by services wrap one of these where a caller may want to
// branch on the failure class.
var (
	// ErrUnsupportedServiceType is returned by New for an unknown service type.
	ErrUnsupportedServiceType = errors.New("unsupported service type")
	// ErrInvalidOptions reports invalid service configuration.
	ErrInvalidOptions = errors.New("invalid notification service options")
	// ErrRecipientNotConfigured reports a recipient that has no configured endpoint/key.
	ErrRecipientNotConfigured = errors.New("recipient is not configured for notification service")
	// ErrInvalidNotification reports a rendered notification that cannot be delivered.
	ErrInvalidNotification = errors.New("invalid notification")
	// ErrURLNotAllowed reports an endpoint rejected by URL validation or the dial guard.
	ErrURLNotAllowed = errors.New("notification endpoint is not allowed")
	// ErrDeliveryFailed reports a delivery rejected by the remote service.
	ErrDeliveryFailed = errors.New("notification delivery failed")
)

// StatusError is returned when a remote endpoint answers with an unexpected HTTP status.
// Body is truncated and redacted.
type StatusError struct {
	Service    string
	Method     string
	URL        string
	StatusCode int
	Body       string
}

func (e *StatusError) Error() string {
	msg := fmt.Sprintf("%s: %s %s returned HTTP %d", e.Service, e.Method, e.URL, e.StatusCode)
	if e.Body != "" {
		msg += ": " + e.Body
	}
	return msg
}

// Unwrap classifies every status error as a delivery failure.
func (e *StatusError) Unwrap() error { return ErrDeliveryFailed }

// Retryable reports whether the status is considered transient.
func (e *StatusError) Retryable() bool { return retryableStatus(e.StatusCode) }
