package apiclient

import (
	"context"
	"net/http"
)

// Route patterns of the notification configuration API (api-contract.md, "Notifications,
// webhooks, badges, extensions").
const (
	PathNotificationServices  = "/api/v1/notifications/services"
	PathNotificationTriggers  = "/api/v1/notifications/triggers"
	PathNotificationTemplates = "/api/v1/notifications/templates"
)

// NotificationService names one configured notification service. Service configuration
// (URLs, tokens, recipients) is never returned.
type NotificationService struct {
	Name string `json:"name"`
}

// NotificationServiceList is the response of GET /api/v1/notifications/services.
type NotificationServiceList struct {
	Items []NotificationService `json:"items"`
}

// NotificationTrigger names one configured trigger.
type NotificationTrigger struct {
	Name string `json:"name"`
	// Description joins the distinct descriptions of the trigger's conditions (Patrol
	// addition; empty when none is set).
	Description string `json:"description,omitempty"`
}

// NotificationTriggerList is the response of GET /api/v1/notifications/triggers.
type NotificationTriggerList struct {
	Items []NotificationTrigger `json:"items"`
}

// NotificationTemplate names one configured template.
type NotificationTemplate struct {
	Name string `json:"name"`
}

// NotificationTemplateList is the response of GET /api/v1/notifications/templates.
type NotificationTemplateList struct {
	Items []NotificationTemplate `json:"items"`
}

// ListNotificationServices lists the configured notification services.
func (c *Client) ListNotificationServices(ctx context.Context) (*NotificationServiceList, error) {
	var out NotificationServiceList
	return &out, c.Do(ctx, http.MethodGet, PathNotificationServices, nil, &out)
}

// ListNotificationTriggers lists the configured notification triggers.
func (c *Client) ListNotificationTriggers(ctx context.Context) (*NotificationTriggerList, error) {
	var out NotificationTriggerList
	return &out, c.Do(ctx, http.MethodGet, PathNotificationTriggers, nil, &out)
}

// ListNotificationTemplates lists the configured notification templates.
func (c *Client) ListNotificationTemplates(ctx context.Context) (*NotificationTemplateList, error) {
	var out NotificationTemplateList
	return &out, c.Do(ctx, http.MethodGet, PathNotificationTemplates, nil, &out)
}
