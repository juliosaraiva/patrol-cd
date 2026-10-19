// Package deploy is the deployment validation harness of Patrol CD's install artifacts: the
// Helm chart (deploy/helm/patrol-cd), the static install manifests (deploy/manifests) and the
// CRDs with the Operation admission policy (deploy/crds).
//
// The tests (package deploy_test) render every chart profile (deploy/helm/patrol-cd/ci) and
// check, from the cheapest to the most expensive:
//
//   - render matrix: helm lint --strict, deterministic helm template, values.schema.json;
//   - wiring: every patrolcd container's arguments parsed by the real cobra command, PATROL_*
//     environment names checked against settings.ParamsForComponent and the documented
//     table, listener ports, probes, Services and mounted files against
//     docs/architecture/components/wiring.md;
//   - API validation: server-side dry run of every rendered object with strict field
//     validation against an envtest kube-apiserver (warnings fail);
//   - Pod Security: every pod template admitted by the "restricted" profile;
//   - RBAC sufficiency: the patrolcd binary run as each component's ServiceAccount with
//     exactly the rendered RBAC, driven through a minimal scenario, with forbidden requests
//     detected in the kube-apiserver audit log and in the component logs;
//   - Helm lifecycle (install, upgrade, rollback, uninstall, re-install) and server-side apply
//     of the static manifests against a real API server;
//   - field ownership across upgrades: trust ConfigMaps changed through the API survive
//     helm upgrade/rollback and a static re-apply; the controller's replica count follows
//     controller.replicas (kubectl scale conflicts, as documented);
//   - installer personas of the namespace-scoped manifests (namespace admin refused; admin
//     with escalate/bind, or cluster-admin bound in the namespace, accepted);
//   - NetworkPolicy egress to the OTLP collector when tracing is configured;
//   - static manifests and chart render the same workloads.
//
// Tools: helm (HELM or PATH; PATROL_REQUIRE_DEPLOY_TOOLS=1 turns a missing tool into a
// failure), kustomize (bin/kustomize, KUSTOMIZE or PATH) where needed. The envtest-backed
// tests run when KUBEBUILDER_ASSETS is set (PATROL_REQUIRE_ENVTEST=1 makes its absence a
// failure); -short skips them. Every skip prints a reason starting with "SKIPPED:".
// Results and commands: docs/verification/runs/2026-10-17-deploy-validation.md.
package deploy
