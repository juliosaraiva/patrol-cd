package services

import (
	"context"
	"fmt"
	"io"
	"sync"

	"sigs.k8s.io/yaml"
)

// TypeConsole is the pseudo service used by "admin notifications template notify" (default
// recipient console:stdout). It is not configurable through the ConfigMap.
const TypeConsole = "console"

type consoleService struct {
	mu sync.Mutex
	w  io.Writer
}

// NewConsole returns a Service that writes the rendered notification to w: the message,
// followed by the service specific sections as YAML when any are set.
func NewConsole(w io.Writer) Service { return &consoleService{w: w} }

func (c *consoleService) Send(_ context.Context, n Notification, _ Destination) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := fmt.Fprintln(c.w, n.Message); err != nil {
		return err
	}
	sections := n
	sections.Message = ""
	if sections.isEmpty() {
		return nil
	}
	b, err := yaml.Marshal(sections)
	if err != nil {
		return fmt.Errorf("console: %w", err)
	}
	_, err = fmt.Fprintf(c.w, "---\n%s", b)
	return err
}

func (n Notification) isEmpty() bool {
	return n.Message == "" && n.Email == nil && n.Slack == nil && n.Mattermost == nil && n.RocketChat == nil &&
		n.Teams == nil && n.TeamsWorkflows == nil && n.GoogleChat == nil && len(n.Webhook) == 0 && n.Opsgenie == nil &&
		n.GitHub == nil && n.PagerDuty == nil && n.PagerDutyV2 == nil && n.NewRelic == nil && n.Alertmanager == nil &&
		n.AwsSqs == nil && n.GCPPubSub == nil
}
