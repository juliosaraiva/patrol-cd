# Patrol CD examples

Runnable examples of every Patrol CD resource kind, source type, ApplicationSet generator,
setting and extension point. The index, the placeholder URLs and how to run and validate them
are in [docs/guides/examples.md](../docs/guides/examples.md).

| Directory | Contents |
|-----------|----------|
| `apps/` | Application sources (each directory is repository content) with the Application or ApplicationSet that deploys them. |
| `platform/` | AppProjects, repositories and credentials, clusters, SCM providers, settings ConfigMaps and Secret templates. |
| `github/` | GitHub App walk-through: pull request preview environments, webhook checklist, commit status and PR comment notifications. |
| `notifications/` | Notification subscriptions on Applications and AppProjects. |
| `plugins/` | Lua health check and resource actions for a CRD (with test fixtures); UI extension skeleton. |

Every object is validated by `go test ./test/examples` against a real API server with the
Patrol CD CRDs, and every source is rendered with the real tools. Secrets contain only
`REPLACE_ME` placeholders.
