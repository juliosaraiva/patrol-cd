# Default notifications catalog

The trigger and template definitions in `triggers/` and `templates/` are data files derived
from the Argo CD v3.5.5 notifications catalog
(`notifications_catalog/` at commit `a5038b0e2bbac1715754380ae0084df8e3d5b48c`,
https://github.com/argoproj/argo-cd/tree/v3.5.5/notifications_catalog).

- Copyright The Argo Authors.
- Licensed under the Apache License, Version 2.0
  (https://www.apache.org/licenses/LICENSE-2.0). Each file keeps an attribution header.
- They are reused unmodified in behavior for parity, except that the template context key
  `argocdUrl` is renamed to `patrolUrl`. The engine aliases the two keys in both directions,
  so configurations written for Argo CD (`context: argocdUrl: ...`) keep working.
- These files must be listed in the repository `NOTICE` file.

`catalog.go` embeds the files and exposes them as notifications ConfigMap entries
(`trigger.<name>` and `template.<name>`) that `notifications.ParseConfig` can merge as
defaults (`notifications.WithDefaultCatalog()`); user-provided keys always win.

Triggers: on-created, on-deleted, on-deployed, on-health-degraded, on-sync-failed,
on-sync-running, on-sync-status-unknown, on-sync-succeeded. Each sends the template with the
matching `app-*` name.
