package app

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/juliosaraiva/patrol-cd/internal/cli/output"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// maxLogReconnects bounds stream re-opens while following logs.
const maxLogReconnects = 100

// logsOptions are the flags of `app logs`.
type logsOptions struct {
	group, kind, namespace, resourceName, container string
	untilTime, filter, appNamespace                 string
	follow, previous, matchCase                     bool
	tail, sinceSeconds                              int64
}

// query builds the log request; only flags that were set are sent.
func (o *logsOptions) query(name, ns string, set func(string) bool) *apiclient.ApplicationLogsQuery {
	q := &apiclient.ApplicationLogsQuery{
		Name: name, AppNamespace: ns, Namespace: o.namespace, Container: o.container, UntilTime: o.untilTime,
		Filter: o.filter, Kind: o.kind, Group: o.group, ResourceName: o.resourceName,
	}
	if o.follow {
		v := true
		q.Follow = &v
	}
	if o.previous {
		v := true
		q.Previous = &v
	}
	if o.matchCase {
		v := true
		q.MatchCase = &v
	}
	if set("tail") {
		v := o.tail
		q.TailLines = &v
	}
	if set("since-seconds") {
		v := o.sinceSeconds
		q.SinceSeconds = &v
	}
	return q
}

func newLogsCommand(e *env) *cobra.Command {
	o := &logsOptions{}
	c := &cobra.Command{
		Use:   "logs APPNAME",
		Short: "Get logs of application pods",
		Example: `  # Get logs of pods associated with the application "my-app"
  patrol app logs my-app

  # Get logs of pods associated with the application "my-app" in a specific resource group
  patrol app logs my-app --group my-group

  # Get logs of pods associated with the application "my-app" in a specific resource kind
  patrol app logs my-app --kind my-kind

  # Get logs of pods associated with the application "my-app" in a specific namespace
  patrol app logs my-app --namespace my-namespace

  # Get logs of pods associated with the application "my-app" for a specific resource name
  patrol app logs my-app --name my-resource

  # Stream logs in real-time for the application "my-app"
  patrol app logs my-app -f

  # Get the last N lines of logs for the application "my-app"
  patrol app logs my-app --tail 100

  # Get logs since a specified number of seconds ago
  patrol app logs my-app --since-seconds 3600

  # Get logs until a specified time (format: "2023-10-10T15:30:00Z")
  patrol app logs my-app --until-time "2023-10-10T15:30:00Z"

  # Filter logs to show only those containing a specific string
  patrol app logs my-app --filter "error"

  # Filter logs to show only those containing a specific string and match case
  patrol app logs my-app --filter "error" --match-case

  # Get logs for a specific container within the pods
  patrol app logs my-app -c my-container

  # Get previously terminated container logs
  patrol app logs my-app -p`,
		RunE: func(c *cobra.Command, args []string) error {
			ctx := cmdContext(c)
			if len(args) == 0 {
				return usage(c, args)
			}
			if o.tail < 0 || o.sinceSeconds < 0 {
				return fatal("--tail and --since-seconds must not be negative")
			}
			cl, err := e.client(ctx)
			if err != nil {
				return err
			}
			name, ns := parseQualifiedName(args[0], o.appNamespace)
			q := o.query(name, ns, c.Flags().Changed)
			out := c.OutOrStdout()
			for attempt := 0; ; attempt++ {
				s, err := cl.StreamApplicationLogs(ctx, q)
				if err != nil {
					return fatalErr(fmt.Errorf("failed to get pod logs: %w", err))
				}
				retry, err := copyLogs(s, func(line string) { _, _ = fmt.Fprintln(out, output.SanitizeMultiline(line)) })
				_ = s.Close()
				if err == nil {
					return nil
				}
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if !retry || !o.follow || attempt >= maxLogReconnects {
					return fatalErr(fmt.Errorf("stream read failed: %w", err))
				}
				// Reconnect and continue from the last second (upstream behavior).
				since := int64(1)
				q.SinceSeconds, q.TailLines = &since, nil
				if err := sleepCtx(ctx, e.watchRetryDelay); err != nil {
					return err
				}
			}
		},
	}
	fs := c.Flags()
	fs.StringVarP(&o.appNamespace, "app-namespace", "N", "", "Namespace of the application")
	fs.StringVar(&o.group, "group", "", "Resource group")
	fs.StringVar(&o.kind, "kind", "", "Resource kind")
	fs.StringVar(&o.namespace, "namespace", "", "Resource namespace")
	fs.StringVar(&o.resourceName, "name", "", "Resource name")
	fs.BoolVarP(&o.follow, "follow", "f", false, "Specify if the logs should be streamed")
	fs.Int64Var(&o.tail, "tail", 0, "The number of lines from the end of the logs to show")
	fs.Int64Var(&o.sinceSeconds, "since-seconds", 0, "A relative time in seconds before the current time from which to show logs")
	fs.StringVar(&o.untilTime, "until-time", "", "Show logs until this time")
	fs.StringVar(&o.filter, "filter", "", "Show logs contain this string")
	fs.StringVarP(&o.container, "container", "c", "", "Optional container name")
	fs.BoolVarP(&o.previous, "previous", "p", false, "Specify if the previously terminated container logs should be returned")
	fs.BoolVarP(&o.matchCase, "match-case", "m", false, "Specify if the filter should be case-sensitive")
	return c
}

// copyLogs writes log entries until the stream ends. retry reports an interruption that
// re-opening the stream may fix (server unavailable or a broken connection).
func copyLogs(s LogStream, write func(string)) (retry bool, err error) {
	for {
		entry, err := s.Next()
		if err != nil {
			if isEOF(err) {
				return false, nil
			}
			if apiclient.IsUnavailable(err) {
				return true, err
			}
			var ae *apiclient.APIError
			if !asAPIError(err, &ae) {
				return true, err
			}
			return false, err
		}
		if entry.Last {
			return false, nil
		}
		write(entry.Content)
	}
}
