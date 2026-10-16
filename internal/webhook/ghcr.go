package webhook

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/juliosaraiva/patrol-cd/pkg/scm"
	"github.com/juliosaraiva/patrol-cd/pkg/scm/github"
)

// ghcrRegistry is the registry host of GitHub Container Registry events.
const ghcrRegistry = "ghcr.io"

// errRegistrySignature marks a GHCR signature failure (answered 401, as upstream).
var errRegistrySignature = errors.New("registry webhook HMAC verification failed")

// registryEvent is a normalized container image publication (REPO-123).
type registryEvent struct {
	// Registry is the registry host and Repository the path within it; together they
	// form the OCI repository URL oci://Registry/Repository.
	Registry   string
	Repository string
	Tag        string
	DeliveryID string
	Verified   bool
}

func (e *registryEvent) ociURL() string { return "oci://" + e.Registry + "/" + e.Repository }

type ghcrPayload struct {
	Action  string `json:"action"`
	Package struct {
		Name        string `json:"name"`
		PackageType string `json:"package_type"`
		Owner       struct {
			Login string `json:"login"`
		} `json:"owner"`
		PackageVersion struct {
			ContainerMetadata struct {
				Tag struct {
					Name string `json:"name"`
				} `json:"tag"`
			} `json:"container_metadata"`
		} `json:"package_version"`
	} `json:"package"`
}

// verifyGHCR checks X-Hub-Signature-256 ("sha256=<hex HMAC-SHA256 of the raw body>")
// against every candidate secret in constant time. Without candidates nothing is
// verified (upstream accepts unsigned package events when webhook.github.secret is
// unset).
func verifyGHCR(h http.Header, body []byte, candidates [][]byte) (verified bool, err error) {
	if len(candidates) == 0 {
		return false, nil
	}
	sig := h.Get(github.HeaderSignature256)
	if sig == "" {
		return false, fmt.Errorf("%w: missing %s header", errRegistrySignature, github.HeaderSignature256)
	}
	for _, s := range candidates {
		if scm.VerifyHMACSHA256(sig, "sha256=", body, s) == nil {
			return true, nil
		}
	}
	return false, fmt.Errorf("%w: signature mismatch", errRegistrySignature)
}

// parseGHCR normalizes a GitHub package event. It returns (nil, nil) for events that are
// skipped on purpose: actions other than published, packages other than containers, and
// versions without a tag.
func parseGHCR(body []byte) (*registryEvent, error) {
	var p ghcrPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("failed to unmarshal GHCR webhook payload: %w", err)
	}
	if p.Action != "published" || !strings.EqualFold(p.Package.PackageType, "container") {
		return nil, nil
	}
	tag := p.Package.PackageVersion.ContainerMetadata.Tag.Name
	if tag == "" || p.Package.Owner.Login == "" || p.Package.Name == "" {
		return nil, nil
	}
	return &registryEvent{
		Registry:   ghcrRegistry,
		Repository: p.Package.Owner.Login + "/" + p.Package.Name,
		Tag:        tag,
	}, nil
}
