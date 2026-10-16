// Package webhooks documents the SCM and registry webhook receiver (POST /api/webhook and
// POST /api/webhook/{provider}) in the OpenAPI document.
//
// The receiver is internal/webhook, mounted by the server wiring (internal/server) as raw
// routes. Those runtime routes are hidden because they accept any method and a
// multi-segment path; the routes registered here are documentation-only
// (rest.Route.DocumentationOnly): rest.Table.Mount never registers them, so this package
// cannot change routing. The receiver names and headers mirror internal/webhook
// (providers.go and the pkg/scm verifiers); service_test.go checks them against the real
// handler.
package webhooks

import (
	"net/http"
	"reflect"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	"github.com/juliosaraiva/patrol-cd/internal/webhook"
)

// Paths of the receiver.
const (
	PathWebhook         = webhook.DefaultPathPrefix
	PathProviderWebhook = webhook.DefaultPathPrefix + "/{provider}"
)

const tag = "webhooks"

// Provider is a receiver name of POST /api/webhook/{provider}: a provider of
// internal/webhook or one of its aliases.
type Provider string

// JSONSchemaEnum lists the accepted receiver names: providers, then aliases.
func (Provider) JSONSchemaEnum() []any {
	var out []any
	for _, r := range Receivers() {
		out = append(out, r.Name)
	}
	for _, r := range Receivers() {
		for _, a := range r.Aliases {
			out = append(out, a)
		}
	}
	return out
}

// ProviderRequest addresses a receiver explicitly.
type ProviderRequest struct {
	// Provider selects the receiver (matched case-insensitively; see
	// x-patrol-webhook-receivers). The delivery's headers must still identify that
	// provider, otherwise the answer is 400. Unknown names get 404.
	Provider Provider `path:"provider" json:"-"`
}

// Receiver documents one webhook receiver (the x-patrol-webhook-receivers extension).
type Receiver struct {
	// Name is the receiver name used in /api/webhook/{provider}.
	Name string `json:"name"`
	// Aliases are other accepted path names.
	Aliases []string `json:"aliases,omitempty"`
	// Kind is "scm" (Git hosting events: pushes, pull requests) or "registry" (container
	// registry package events).
	Kind string `json:"kind"`
	// RequiredHeaders identify the provider's deliveries; at least these must be present
	// (also when the provider is named in the path).
	RequiredHeaders []string `json:"requiredHeaders"`
	// ExcludedHeaders must be absent (they identify another provider).
	ExcludedHeaders []string `json:"excludedHeaders,omitempty"`
	// RequiredHeaderValues constrain required headers to values.
	RequiredHeaderValues map[string]string `json:"requiredHeaderValues,omitempty"`
	// ExcludedHeaderValues are header values that identify another provider.
	ExcludedHeaderValues map[string]string `json:"excludedHeaderValues,omitempty"`
	// Authentication names the header (or scheme) checked against the secret when one is
	// configured.
	Authentication string `json:"authentication"`
	// Secrets are the patrol-secret keys that verify deliveries.
	Secrets []string `json:"secrets,omitempty"`
	// SCMProviderTypes are the SCMProvider spec.type values whose spec.webhook.secretRef
	// also verifies deliveries; for kind "scm" they map an SCMProvider to the receiver of
	// its webhook URL.
	SCMProviderTypes []v1alpha1.SCMProviderType `json:"scmProviderTypes,omitempty"`
}

// Receivers returns the receivers in internal/webhook's detection order (the first whose
// headers match a delivery to /api/webhook handles it).
func Receivers() []Receiver {
	return []Receiver{
		{
			Name: webhook.ProviderAzureDevOps, Aliases: []string{"azure-devops"}, Kind: "scm",
			RequiredHeaders:  []string{"X-Vss-Activityid"},
			Authentication:   "HTTP Basic credentials (Authorization header)",
			Secrets:          []string{"webhook.azuredevops.username", "webhook.azuredevops.password"},
			SCMProviderTypes: []v1alpha1.SCMProviderType{v1alpha1.SCMProviderTypeAzureDevOps},
		},
		{
			Name: webhook.ProviderGitea, Kind: "scm",
			RequiredHeaders:  []string{"X-Gitea-Event"},
			Authentication:   "X-Gitea-Signature (hex HMAC-SHA256 of the body; X-Gogs-Signature or X-Hub-Signature-256 when absent)",
			Secrets:          []string{"webhook.gogs.secret"},
			SCMProviderTypes: []v1alpha1.SCMProviderType{v1alpha1.SCMProviderTypeGitea},
		},
		{
			Name: webhook.ProviderGogs, Kind: "scm",
			RequiredHeaders: []string{"X-Gogs-Event"},
			ExcludedHeaders: []string{"X-Gitea-Event"},
			Authentication:  "X-Gogs-Signature (hex HMAC-SHA256 of the body)",
			Secrets:         []string{"webhook.gogs.secret"},
		},
		{
			Name: webhook.ProviderGitHub, Kind: "scm",
			RequiredHeaders:      []string{"X-GitHub-Event"},
			ExcludedHeaders:      []string{"X-Gogs-Event", "X-Gitea-Event"},
			ExcludedHeaderValues: map[string]string{"X-GitHub-Event": "package"},
			Authentication:       "X-Hub-Signature-256 (sha256=<hex HMAC-SHA256 of the body>)",
			Secrets:              []string{"webhook.github.secret"},
			SCMProviderTypes:     []v1alpha1.SCMProviderType{v1alpha1.SCMProviderTypeGitHub},
		},
		{
			Name: webhook.ProviderGitLab, Kind: "scm",
			RequiredHeaders:  []string{"X-Gitlab-Event"},
			Authentication:   "X-Gitlab-Token (the secret token)",
			Secrets:          []string{"webhook.gitlab.secret"},
			SCMProviderTypes: []v1alpha1.SCMProviderType{v1alpha1.SCMProviderTypeGitLab},
		},
		{
			Name: webhook.ProviderBitbucketCloud, Aliases: []string{"bitbucketcloud", "bitbucket-cloud"}, Kind: "scm",
			RequiredHeaders:  []string{"X-Hook-UUID"},
			Authentication:   "X-Hook-UUID (the webhook's UUID)",
			Secrets:          []string{"webhook.bitbucket.uuid"},
			SCMProviderTypes: []v1alpha1.SCMProviderType{v1alpha1.SCMProviderTypeBitbucketCloud},
		},
		{
			Name: webhook.ProviderBitbucketServer, Aliases: []string{"bitbucket-server"}, Kind: "scm",
			RequiredHeaders:  []string{"X-Event-Key"},
			ExcludedHeaders:  []string{"X-Hook-UUID"},
			Authentication:   "X-Hub-Signature (sha256=<hex HMAC-SHA256 of the body>)",
			Secrets:          []string{"webhook.bitbucketserver.secret"},
			SCMProviderTypes: []v1alpha1.SCMProviderType{v1alpha1.SCMProviderTypeBitbucketServer},
		},
		{
			Name: webhook.ProviderGHCR, Kind: "registry",
			RequiredHeaders:      []string{"X-GitHub-Event"},
			RequiredHeaderValues: map[string]string{"X-GitHub-Event": "package"},
			Authentication:       "X-Hub-Signature-256 (sha256=<hex HMAC-SHA256 of the body>)",
			Secrets:              []string{"webhook.github.secret"},
			SCMProviderTypes:     []v1alpha1.SCMProviderType{v1alpha1.SCMProviderTypeGitHub},
		},
	}
}

// Service registers the documentation-only receiver routes.
type Service struct{}

// New returns the service.
func New() *Service { return &Service{} }

// Register adds POST /api/webhook and POST /api/webhook/{provider} as documentation-only
// routes.
func (*Service) Register(t *rest.Table) {
	access := rest.Public()
	access.Note = "Patrol credentials are ignored: deliveries are verified with the provider's webhook secret " +
		"(patrol-secret webhook.* keys, or spec.webhook.secretRef of an SCMProvider in a trusted namespace). " +
		"Unsigned deliveries are accepted only while no webhook secret is configured."
	text := "text/plain"
	retry := []string{"Retry-After"}
	common := []rest.ResponseDoc{
		{Status: http.StatusOK, Description: "Accepted (queued), ignored (an event that refreshes nothing) or a duplicate delivery; empty body."},
		{Status: http.StatusBadRequest, ContentType: text, Description: "Unknown event (no provider matches the headers, or not the named one), invalid or too large payload, failed signature or token verification, unsupported event."},
		{Status: http.StatusUnauthorized, ContentType: text, Description: "Registry (ghcr) delivery whose signature verification failed."},
		{Status: http.StatusTooManyRequests, ContentType: text, Headers: retry, Description: "Too many deliveries from the source."},
		{Status: http.StatusServiceUnavailable, ContentType: text, Headers: retry, Description: "Busy (request body budget or event queue full), or the webhook secrets are not loaded yet or unavailable."},
	}
	body := "The provider's event payload (JSON) as sent, at most webhook.maxPayloadSizeMB (default 50 MB, clamped to 25 MiB)."
	ext := map[string]any{"x-patrol-webhook-receivers": Receivers()}
	t.Add(rest.Route{
		Method: http.MethodPost, Path: PathWebhook, OperationID: "receiveWebhook",
		Summary: "Receive an SCM or registry webhook (provider detected from the headers)", Tags: []string{tag},
		Description: "Detects the provider from the delivery's headers (x-patrol-webhook-receivers, in order), verifies it and refreshes the matching Applications and ApplicationSets asynchronously. " +
			"SCM providers are configured with the URL <server>/api/webhook or <server>/api/webhook/{provider}.",
		Access: access, SkipAuth: true, Audit: rest.AuditNever, DocumentationOnly: true,
		RequestContentType: "application/json", RequestDescription: body,
		Responses: common, Extensions: ext,
	})
	t.Add(rest.Route{
		Method: http.MethodPost, Path: PathProviderWebhook, OperationID: "receiveProviderWebhook",
		Summary: "Receive a webhook for a named provider", Tags: []string{tag},
		Description: "Same as POST /api/webhook with the receiver named in the path; the delivery's headers must identify that provider.",
		Access:      access, SkipAuth: true, Audit: rest.AuditNever, DocumentationOnly: true,
		Request:            reflect.TypeFor[ProviderRequest](),
		RequestContentType: "application/json", RequestDescription: body,
		Responses: append(append([]rest.ResponseDoc(nil), common...),
			rest.ResponseDoc{Status: http.StatusNotFound, ContentType: text, Description: "Unknown receiver name."}),
		Extensions: ext,
	})
}
