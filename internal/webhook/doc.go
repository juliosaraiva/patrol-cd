// Package webhook ingests SCM and registry webhooks for patrol-server (POST /api/webhook
// and /api/webhook/{provider}) and turns them into Kubernetes-native refresh requests:
//
//   - Applications whose sources (single source, every source of a multi-source
//     Application including ref-only sources, and the hydrator's dry and sync sources)
//     match the pushed repository, revision and manifest-generate-paths get the
//     cd.patrol.example/refresh=normal annotation (plus cd.patrol.example/hydrate=normal
//     when the dry source matched);
//   - ApplicationSets with a matching Git or Pull Request generator (top level or nested in
//     Matrix/Merge), or a top-level Plugin generator, get
//     cd.patrol.example/application-set-refresh=true.
//
// Request flow (ServeHTTP): per-source rate limit (429) → provider detection from headers
// or the path (400 "Unknown webhook event") → method check (405) → raw body read with the
// webhook.maxPayloadSizeMB cap (clamped to the 25 MiB the verifiers accept) within a
// global in-flight body budget (503 when exhausted), before any parsing → unsigned policy
// (once any webhook secret is configured, unsigned deliveries are rejected for every
// provider) → signature/token verification by the pkg/scm WebhookVerifier of the provider
// over the exact raw bytes, with the secrets of patrol-secret and of SCMProvider objects →
// normalization to scm.Event → delivery-ID de-duplication (verified deliveries only) →
// bounded asynchronous queue (503 when full). Workers match Applications and
// ApplicationSets and hand refresh requests to a de-duplicating, optionally jittered
// refresh queue whose workers apply JSON merge patches (conditional on the
// resourceVersion read for Applications) with conflict retries. Every effect is
// idempotent; polling stays the recovery path, so a lost webhook only delays a refresh.
// Untrusted payload values are clipped before they are logged.
//
// The handler holds no authoritative state: the delivery cache, rate limiters and queues
// are in memory, per replica, and bounded.
//
// See docs/architecture/components/webhook.md for behavior, parity notes and differences
// from Argo CD v3.5.5.
package webhook
