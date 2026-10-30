# GitHub integration walk-through

Pull request preview environments for `example-org/guestbook` (the repository content is
`examples/apps/helm-chart`: the chart lives in `chart/`), with a GitHub App as the only
identity, webhooks for immediate refreshes, and sync results reported back to the pull
request.

| File | What it does |
|------|--------------|
| `01-scmprovider.yaml` | `SCMProvider` for the GitHub App (API, webhook secret) and a `RepositoryCredential` that clones with App installation tokens. Its `statusReporting` block is validated but not wired yet (#235). |
| `02-appproject-previews.yaml` | `AppProject previews`: one repository, namespaces `guestbook-pr-*` only. |
| `03-applicationset-previews.yaml` | Pull request generator with `providerRef`: one Application per labelled PR, namespace per PR, cleanup on close. |
| `04-notifications.yaml` | Notification templates and triggers: commit status + upserted PR comment. Merge into `patrol-notifications-cm`; it only adds keys (the `github` service and `context` stay those of the platform ConfigMap). |

Apply order: the platform settings (`examples/platform/settings`), then these files in
numeric order. Every Secret contains `REPLACE_ME` placeholders: create the real Secrets out
of band and never commit them.

## 1. Create the GitHub App

GitHub: *Organization settings → Developer settings → GitHub Apps → New GitHub App*.

- Homepage URL: `https://patrol.example.com`.
- Webhook URL: `https://patrol.example.com/api/webhook`, webhook secret: a random value
  (store it in `example-org-github-webhook`, key `secret`).
- Repository permissions (least privilege; the full matrix is in
  [`docs/integrations/github-permissions.md`](../../docs/integrations/github-permissions.md)):

  | Permission | Access | Needed for |
  |------------|--------|------------|
  | Metadata | Read-only | always |
  | Contents | Read-only | cloning, pull request head commits |
  | Pull requests | Read and write | pull request generator (read), PR comments (write) |
  | Commit statuses | Read and write | `github.status` notifications |

  Checks is not needed: this example sends no `github.checkRun` notification, and the
  SCMProvider's `statusReporting` reports nothing yet (#235). Once #235 wires it, its
  `mode: checks` will need Checks: Read and write.

- Subscribe to events: `Push`, `Pull request`.
- Install the App on *Only select repositories* → `guestbook`. Note the App ID
  (`appID` in the manifests) and generate a private key (`example-org-github-app`, key
  `privateKey`; `github-privateKey` in `patrol-notifications-secret`).

## 2. Webhook registration checklist

Patrol CD serves one endpoint for Applications and ApplicationSets, so a single webhook
(on the App, or per repository/organization) is enough.

- [ ] Payload URL `https://<patrol-server>/api/webhook` (or `/api/webhook/github`), reachable
      from GitHub (`api.github.com/meta` lists the hook source ranges).
- [ ] Content type `application/json`.
- [ ] Secret set, identical to the `SCMProvider` `spec.webhook.secretRef` value (or to
      `webhook.github.secret` in `patrol-secret`). Once any secret is configured, unsigned
      deliveries are rejected.
- [ ] SSL verification enabled; the server certificate chains to a public CA or GitHub
      Enterprise trusts it.
- [ ] Events: `push` (Application refresh, Git generators) and `pull_request` (pull
      request generator; `opened`, `synchronize`, `reopened`, `closed`, `labeled`,
      `unlabeled`).
- [ ] After saving, *Recent Deliveries* shows the `ping` delivery answered with `200`.
- [ ] Push a commit and check that the matching Application's
      `cd.patrol.example/refresh` annotation was set and removed; label a pull request
      `preview` and check that `guestbook-pr-<number>` appears within seconds.
- [ ] Payloads above `webhook.maxPayloadSizeMB` (patrol-cm, default 50, capped at 25 MiB)
      are rejected; keep the default unless deliveries are refused.

Nothing in this directory registers a webhook or an App; the steps above are manual.

## 3. Deploy

With the kind environment (`eval "$(hack/kind/env.sh)"`, see docs/guides/examples.md):

```bash
k() { kubectl --kubeconfig "$PATROL_CONTROL_KUBECONFIG" --context "$PATROL_CONTROL_CONTEXT" "$@"; }
k apply --server-side -f examples/github/01-scmprovider.yaml
k apply --server-side -f examples/github/02-appproject-previews.yaml
k apply --server-side -f examples/github/03-applicationset-previews.yaml
k -n patrol patch configmap patrol-notifications-cm --type merge \
  --patch-file examples/github/04-notifications.yaml
```

## 4. Lifecycle of a preview

1. A pull request labelled `preview` is opened → `pull_request` webhook → the
   ApplicationSet is refreshed → `guestbook-pr-<number>` is created (project `previews`,
   namespace `guestbook-pr-<number>`, head commit).
2. Every push to the branch (`synchronize`) changes `head_sha` → the Application's
   `targetRevision` is updated and auto-sync deploys it.
3. After each sync, `on-preview-deployed`/`on-preview-failed` post a commit status
   (`patrol-cd/preview`) and update the single PR comment tagged `patrol-preview`. No check
   run is reported: the SCMProvider's `statusReporting` is not wired yet (#235).
4. Closing/merging the pull request (or removing the label) removes it from the generator
   output → the Application is deleted → the resources finalizer deletes the workload and
   the `Namespace` rendered by the chart.
