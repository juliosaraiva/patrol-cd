package webhook

import (
	"net/http"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
	"github.com/juliosaraiva/patrol-cd/pkg/scm"
	"github.com/juliosaraiva/patrol-cd/pkg/scm/azuredevops"
	"github.com/juliosaraiva/patrol-cd/pkg/scm/bitbucketcloud"
	"github.com/juliosaraiva/patrol-cd/pkg/scm/bitbucketserver"
	"github.com/juliosaraiva/patrol-cd/pkg/scm/gitea"
	"github.com/juliosaraiva/patrol-cd/pkg/scm/github"
	"github.com/juliosaraiva/patrol-cd/pkg/scm/gitlab"
	"github.com/juliosaraiva/patrol-cd/pkg/scm/gogs"
)

// Provider names, used in /api/webhook/{provider} paths and as the provider metric label.
const (
	ProviderAzureDevOps     = "azuredevops"
	ProviderGitea           = "gitea"
	ProviderGogs            = "gogs"
	ProviderGitHub          = "github"
	ProviderGitLab          = "gitlab"
	ProviderBitbucketCloud  = "bitbucket"
	ProviderBitbucketServer = "bitbucketserver"
	ProviderGHCR            = "ghcr"

	providerUnknown = "unknown"
)

// provider describes one webhook source.
type provider struct {
	name string
	// verifier is nil for registry providers (GHCR), which have their own parser.
	verifier scm.WebhookVerifier
	matches  func(http.Header) bool
	// secret returns the patrol-secret value for this provider (empty when unset).
	secret func(settings.Webhook) []byte
	// scmTypes lists the SCMProvider types whose webhook.secretRef also verifies this
	// provider's deliveries.
	scmTypes []v1alpha1.SCMProviderType
}

func sensitiveBytes(s settings.Sensitive) []byte {
	if s.IsZero() {
		return nil
	}
	return s.Bytes()
}

// providers is the detection order. As upstream, Azure DevOps is checked first and Gogs
// before GitHub (Gogs and Gitea also send GitHub headers; the GitHub verifier excludes
// them). Gitea deliveries (which also carry Gogs headers) are claimed by the Gitea
// verifier and, as upstream treats them as Gogs deliveries, verified with
// webhook.gogs.secret. Bitbucket Cloud is recognized by X-Hook-UUID and Bitbucket Server
// by X-Event-Key without it. GHCR package events come last.
var providers = []*provider{
	{
		name:     ProviderAzureDevOps,
		verifier: azuredevops.WebhookVerifier{},
		secret: func(w settings.Webhook) []byte {
			return azuredevops.JoinSecret(w.AzureDevOpsUsername.Reveal(), w.AzureDevOpsPassword.Reveal())
		},
		scmTypes: []v1alpha1.SCMProviderType{v1alpha1.SCMProviderTypeAzureDevOps},
	},
	{
		name:     ProviderGitea,
		verifier: gitea.WebhookVerifier{},
		secret:   func(w settings.Webhook) []byte { return sensitiveBytes(w.GogsSecret) },
		scmTypes: []v1alpha1.SCMProviderType{v1alpha1.SCMProviderTypeGitea},
	},
	{
		name:     ProviderGogs,
		verifier: gogs.WebhookVerifier{},
		secret:   func(w settings.Webhook) []byte { return sensitiveBytes(w.GogsSecret) },
	},
	{
		name:     ProviderGitHub,
		verifier: github.WebhookVerifier{},
		secret:   func(w settings.Webhook) []byte { return sensitiveBytes(w.GitHubSecret) },
		scmTypes: []v1alpha1.SCMProviderType{v1alpha1.SCMProviderTypeGitHub},
	},
	{
		name:     ProviderGitLab,
		verifier: gitlab.WebhookVerifier{},
		secret:   func(w settings.Webhook) []byte { return sensitiveBytes(w.GitLabSecret) },
		scmTypes: []v1alpha1.SCMProviderType{v1alpha1.SCMProviderTypeGitLab},
	},
	{
		name:     ProviderBitbucketCloud,
		verifier: bitbucketcloud.WebhookVerifier{},
		secret:   func(w settings.Webhook) []byte { return sensitiveBytes(w.BitbucketUUID) },
		scmTypes: []v1alpha1.SCMProviderType{v1alpha1.SCMProviderTypeBitbucketCloud},
	},
	{
		name:     ProviderBitbucketServer,
		verifier: bitbucketserver.WebhookVerifier{},
		secret:   func(w settings.Webhook) []byte { return sensitiveBytes(w.BitbucketServerSecret) },
		scmTypes: []v1alpha1.SCMProviderType{v1alpha1.SCMProviderTypeBitbucketServer},
	},
	{
		name:     ProviderGHCR,
		matches:  func(h http.Header) bool { return h.Get(github.HeaderEvent) == "package" },
		secret:   func(w settings.Webhook) []byte { return sensitiveBytes(w.GitHubSecret) },
		scmTypes: []v1alpha1.SCMProviderType{v1alpha1.SCMProviderTypeGitHub},
	},
}

// providerAliases maps accepted /api/webhook/{provider} path names to providers.
var providerAliases = map[string]string{
	"bitbucketcloud":   ProviderBitbucketCloud,
	"bitbucket-cloud":  ProviderBitbucketCloud,
	"bitbucket-server": ProviderBitbucketServer,
	"azure-devops":     ProviderAzureDevOps,
}

func (p *provider) matchesHeaders(h http.Header) bool {
	if p.verifier != nil {
		return p.verifier.MatchesWebhook(h)
	}
	return p.matches(h)
}

// detectProvider returns the first provider whose headers match.
func detectProvider(h http.Header) *provider {
	for _, p := range providers {
		if p.matchesHeaders(h) {
			return p
		}
	}
	return nil
}

// providerByName resolves a path provider name.
func providerByName(name string) *provider {
	if alias, ok := providerAliases[name]; ok {
		name = alias
	}
	for _, p := range providers {
		if p.name == name {
			return p
		}
	}
	return nil
}

// pushWebURLs returns the repository URLs of a push event that Application and Git
// generator repoURLs are matched against, per provider as upstream: the web URL for
// GitHub, GitLab, Gogs, Gitea and Bitbucket Cloud, both clone links for Bitbucket Server
// (each matched separately; they keep their .git suffix), and the remote URL for Azure
// DevOps.
func pushWebURLs(ev *scm.Event) []string {
	r := ev.Repository
	if r == nil {
		return nil
	}
	var urls []string
	add := func(u string) {
		if u != "" {
			urls = append(urls, u)
		}
	}
	switch ev.Provider {
	case bitbucketserver.Type:
		add(r.CloneURL)
		add(r.SSHURL)
	case azuredevops.Type:
		if r.CloneURL != "" {
			add(r.CloneURL)
		} else {
			add(r.HTMLURL)
		}
	default:
		add(r.HTMLURL)
	}
	return urls
}
