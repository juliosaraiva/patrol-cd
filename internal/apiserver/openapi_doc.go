package apiserver

import (
	"fmt"
	"path/filepath"

	"github.com/juliosaraiva/patrol-cd/internal/apiserver/openapi"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/services/applications"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/services/applicationsets"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/services/badge"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/services/clusters"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/services/extensions"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/services/notifications"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/services/projects"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/services/repositories"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/services/webhooks"
)

// OpenAPI document metadata.
const (
	OpenAPITitle   = "Patrol CD API"
	OpenAPIVersion = "v1"
	// OpenAPIPath is the checked-in document, relative to the repository root.
	OpenAPIPath = "api/openapi/openapi.json"
	// CRDDir holds the generated CRD manifests, relative to the repository root.
	CRDDir = "deploy/crds"
)

const openAPIDescription = "Public HTTP API of patrol-server (docs/architecture/api-contract.md). " +
	"Errors are RFC 9457 problem documents with a stable `code`; streams are Server-Sent Events. " +
	"Every operation documents its access rule in `x-patrol-access` and, for RBAC checks, `x-patrol-rbac`."

// DocumentedServices are the route registrars included in the OpenAPI document besides
// the core routes. Resource API packages add themselves here (registered with zero
// dependencies: only route metadata is used). webhooks only documents the SCM webhook
// receiver, which the server wiring mounts itself: its routes are documentation-only
// (rest.Route.DocumentationOnly) and never mounted.
func DocumentedServices() []Service {
	return []Service{
		clusters.New(clusters.Deps{}),
		projects.New(projects.Deps{}),
		applications.New(applications.Deps{}),
		applicationsets.New(applicationsets.Deps{}),
		notifications.New(notifications.Deps{}),
		badge.New(badge.Deps{}),
		extensions.New(extensions.Deps{}),
		repositories.New(repositories.Deps{}),
		webhooks.New(),
	}
}

// OpenAPIDocument generates the OpenAPI 3.1 document of the public API from the route
// table; root is the repository root (CRD schemas and doc comments are read from it).
func OpenAPIDocument(root string) ([]byte, error) {
	crds, err := openapi.LoadCRDSchemas(filepath.Join(root, CRDDir))
	if err != nil {
		return nil, fmt.Errorf("load CRD schemas: %w", err)
	}
	t, err := NewRouteTable(DocumentedServices()...)
	if err != nil {
		return nil, err
	}
	return openapi.Generate(t.Routes(), openapi.Options{
		Title:       OpenAPITitle,
		Version:     OpenAPIVersion,
		Description: openAPIDescription,
		CRDs:        crds,
		CommentDirs: map[string]string{
			"github.com/juliosaraiva/patrol-cd/pkg/apiclient":                        filepath.Join(root, "pkg", "apiclient"),
			"github.com/juliosaraiva/patrol-cd/internal/apiserver/core":              filepath.Join(root, "internal", "apiserver", "core"),
			"github.com/juliosaraiva/patrol-cd/internal/apiserver/services/webhooks": filepath.Join(root, "internal", "apiserver", "services", "webhooks"),
		},
	})
}
