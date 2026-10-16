package apiclient

import (
	"context"
	"fmt"
	"io"
	"net/http"
)

// PathBadge is the status badge route (outside /api/v1, as upstream).
const PathBadge = "/api/badge"

// BadgeContentType is the media type of status badges.
const BadgeContentType = "image/svg+xml"

// maxBadgeBytes bounds a badge read by the client.
const maxBadgeBytes = 1 << 20

// BadgeRequest is GET /api/badge. The badge is public when statusbadge.enabled is true.
type BadgeRequest struct {
	// Name is the Application.
	Name string `query:"name" json:"-"`
	// AppNamespace is the Application namespace (default: the control-plane namespace).
	// Namespace is the upstream spelling, accepted as an alias.
	AppNamespace string `query:"appNamespace" json:"-"`
	Namespace    string `query:"namespace" json:"-"`
	// Projects aggregates every Application of these projects (repeatable "project").
	Projects []string `query:"project" json:"-"`
	// Revision shows the last synced revision (7 characters unless KeepFullRevision).
	Revision         bool `query:"revision" json:"-"`
	KeepFullRevision bool `query:"keepFullRevision" json:"-"`
	// ShowAppName adds a title row with the Application name.
	ShowAppName bool `query:"showAppName" json:"-"`
	// Width overrides the badge width in pixels.
	Width *int `query:"width" json:"-"`
}

// Badge returns the SVG status badge.
func (c *Client) Badge(ctx context.Context, req *BadgeRequest) ([]byte, error) {
	resp, err := c.DoRaw(ctx, http.MethodGet, PathBadge, req, nil, "")
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBadgeBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read badge: %w", err)
	}
	if len(data) > maxBadgeBytes {
		return nil, fmt.Errorf("badge exceeds %d bytes", maxBadgeBytes)
	}
	return data, nil
}
