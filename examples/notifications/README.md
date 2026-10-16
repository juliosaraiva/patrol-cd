# Notifications

- Services, templates, triggers and default subscriptions: `examples/platform/settings/
  patrol-notifications-cm.yaml` (+ `patrol-notifications-secret.yaml`).
- GitHub commit statuses and pull request comments: `examples/github/04-notifications.yaml`.
- Subscriptions by annotation: `application-subscriptions.yaml` (one Application) and
  `appproject-subscriptions.yaml` (every Application of a project).

Recipients of the annotations are per service: Slack channel names, e-mail addresses; the
webhook (`deploy-tracker`) and GitHub services take no recipient. Subscribe a webhook service
only to triggers whose templates have a `webhook.<service>` section (here
`on-sync-finished` → `deploy-tracker-event`, already subscribed for every Application in
`patrol-notifications-cm`): the catalog templates have none, and without it the webhook
service sends a GET request with no body.

The delivery state is kept in the Application annotation
`notified.notifications.cd.patrol.example`, so a condition is notified once (per `oncePer`
value) across controller restarts.

Built-in catalog triggers: `on-created`, `on-deleted`, `on-deployed`, `on-health-degraded`,
`on-sync-failed`, `on-sync-running`, `on-sync-status-unknown`, `on-sync-succeeded`.
