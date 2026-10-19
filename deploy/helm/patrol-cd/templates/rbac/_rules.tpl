{{/*
RBAC rule sets, derived from the Kubernetes API calls of each component (internal/server wiring,
settings.Manager and repository/cluster listers: list/watch of ConfigMaps and Secrets in the
control-plane namespace; informer caches of the Patrol kinds; event recorders; Lease clients)
and cross-checked with docs/architecture/components/*.md. Comments name the callers.
*/}}

{{/* Events written by the component's recorders (core/v1 broadcaster and controller-runtime). */}}
{{- define "patrol.rules.events" -}}
- apiGroups: [""]
  resources: [events]
  verbs: [create, patch]
- apiGroups: [events.k8s.io]
  resources: [events]
  verbs: [create, patch]
{{- end -}}

{{/* patrol-server, control-plane namespace. */}}
{{- define "patrol.rules.server.controlPlane" -}}
# settings.Manager, repository/cluster registries (list/watch), API writes of repository and
# cluster credential Secrets, account tokens, session revocations, EnsureServerSecretKey.
- apiGroups: [""]
  resources: [secrets]
  verbs: [get, list, watch, create, update, delete]
# settings.Manager; certificate/GPG/known-hosts API writes; patrol-cmd-params-cm at startup.
- apiGroups: [""]
  resources: [configmaps]
  verbs: [get, list, watch, create, update]
# Audit Events; Application events listing.
- apiGroups: [""]
  resources: [events]
  verbs: [create, patch, list]
# Tree API routing: shard Lease holders.
- apiGroups: [coordination.k8s.io]
  resources: [leases]
  verbs: [get]
# Project, cluster, repository and credential APIs; default project; token normalization.
- apiGroups: [cd.patrol.example]
  resources: [appprojects, clusters, repositories, repositorycredentials]
  verbs: [get, list, watch, create, update, patch, delete]
- apiGroups: [cd.patrol.example]
  resources: [appprojects/status, clusters/status, repositories/status, repositorycredentials/status]
  verbs: [update, patch]
{{ include "patrol.rules.server.applications" . }}
{{ include "patrol.rules.server.applicationSets" . }}
{{- end -}}

{{/* patrol-server, every Application namespace. */}}
{{- define "patrol.rules.server.applications" -}}
- apiGroups: [cd.patrol.example]
  resources: [applications]
  verbs: [get, list, watch, create, update, patch, delete]
# Sync/rollback/terminate create and patch Operations on behalf of the authenticated user:
# "impersonate" satisfies the operations.cd.patrol.example admission policy.
- apiGroups: [cd.patrol.example]
  resources: [operations]
  verbs: [get, list, watch, create, patch, impersonate]
- apiGroups: [""]
  resources: [events]
  verbs: [create, patch, list]
{{- end -}}

{{/* patrol-server, every ApplicationSet namespace. */}}
{{- define "patrol.rules.server.applicationSets" -}}
- apiGroups: [cd.patrol.example]
  resources: [applicationsets, scmproviders]
  verbs: [get, list, watch, create, update, patch, delete]
# Generators of the ApplicationSet API (dry runs) read SCM token Secrets.
- apiGroups: [""]
  resources: [secrets]
  verbs: [get]
{{- end -}}

{{/*
patrol-server on the local cluster: the in-cluster destination is reached with the server's own
identity (clusters.Registry, pod configuration) for live resource views, patches, deletions,
resource actions, events, logs and the web terminal.
*/}}
{{- define "patrol.rules.server.localCluster" -}}
- apiGroups: ["*"]
  resources: ["*"]
  verbs: [get, patch, delete]
- apiGroups: [""]
  resources: [events]
  verbs: [list]
- apiGroups: [""]
  resources: [pods, pods/log]
  verbs: [get]
# Resource actions that create Jobs (e.g. CronJob "create-job").
- apiGroups: [batch]
  resources: [jobs]
  verbs: [create]
{{- if .Values.rbac.server.exec }}
- apiGroups: [""]
  resources: [pods/exec]
  verbs: [create]
{{- end }}
{{- if .Values.rbac.server.impersonation }}
- apiGroups: [""]
  resources: [serviceaccounts]
  verbs: [impersonate]
{{- end }}
{{- with .Values.rbac.server.extraClusterRules }}
{{ toYaml . }}
{{- end }}
{{- end -}}

{{/* Application controller, control-plane namespace. */}}
{{- define "patrol.rules.controller.controlPlane" -}}
# settings.Manager, cluster and repository registries; patrol-cmd-params-cm at startup.
- apiGroups: [""]
  resources: [secrets, configmaps]
  verbs: [get, list, watch]
{{ include "patrol.rules.events" . }}
# Shard Leases patrol-controller-shard-<n> (ADR-0008).
- apiGroups: [coordination.k8s.io]
  resources: [leases]
  verbs: [get, list, watch, create, update]
- apiGroups: [cd.patrol.example]
  resources: [appprojects, repositories, repositorycredentials, scmproviders]
  verbs: [get, list, watch]
# Cluster status (assigned shard, connection state) and the Kubernetes version label.
- apiGroups: [cd.patrol.example]
  resources: [clusters]
  verbs: [get, list, watch, patch]
- apiGroups: [cd.patrol.example]
  resources: [clusters/status]
  verbs: [update, patch]
{{- if .Values.controller.sharding.dynamicClusterDistribution }}
# Dynamic cluster distribution reads the replica count of its StatefulSet.
- apiGroups: [apps]
  resources: [statefulsets]
  resourceNames: [{{ include "patrol.controllerName" . }}]
  verbs: [get]
{{- end }}
{{ include "patrol.rules.controller.applications" . }}
{{- end -}}

{{/* Application controller, every Application namespace. */}}
{{- define "patrol.rules.controller.applications" -}}
- apiGroups: [cd.patrol.example]
  resources: [applications]
  verbs: [get, list, watch, update, patch]
- apiGroups: [cd.patrol.example]
  resources: [applications/status]
  verbs: [update, patch]
# Owner references from Operations to Applications (OwnerReferencesPermissionEnforcement).
- apiGroups: [cd.patrol.example]
  resources: [applications/finalizers]
  verbs: [update]
# Automated syncs create Operations initiated by the controller ("impersonate" for the
# admission policy); retention deletes old ones.
- apiGroups: [cd.patrol.example]
  resources: [operations]
  verbs: [get, list, watch, create, update, patch, delete, impersonate]
- apiGroups: [cd.patrol.example]
  resources: [operations/status]
  verbs: [update, patch]
- apiGroups: [""]
  resources: [events]
  verbs: [create, patch]
{{- end -}}

{{/* ApplicationSet controller, control-plane namespace. */}}
{{- define "patrol.rules.appset.controlPlane" -}}
# settings.Manager and repository lister; generator credentials (read live).
- apiGroups: [""]
  resources: [secrets, configmaps]
  verbs: [get, list, watch]
{{ include "patrol.rules.events" . }}
- apiGroups: [cd.patrol.example]
  resources: [appprojects, clusters, repositories, repositorycredentials]
  verbs: [get, list, watch]
# Leader election (controller-runtime Lease 58ac56fa.applicationsets.cd.patrol.example).
- apiGroups: [coordination.k8s.io]
  resources: [leases]
  verbs: [create]
- apiGroups: [coordination.k8s.io]
  resources: [leases]
  resourceNames: [58ac56fa.applicationsets.cd.patrol.example]
  verbs: [get, update]
{{ include "patrol.rules.appset.applicationSets" . }}
{{- with .Values.rbac.appsetControllerExtraRules }}
{{ toYaml . }}
{{- end }}
{{- end -}}

{{/* ApplicationSet controller, every ApplicationSet namespace. */}}
{{- define "patrol.rules.appset.applicationSets" -}}
- apiGroups: [cd.patrol.example]
  resources: [applicationsets]
  verbs: [get, list, watch, create, update, patch, delete]
- apiGroups: [cd.patrol.example]
  resources: [applicationsets/status]
  verbs: [get, update, patch]
- apiGroups: [cd.patrol.example]
  resources: [applicationsets/finalizers]
  verbs: [update]
- apiGroups: [cd.patrol.example]
  resources: [applications]
  verbs: [get, list, watch, create, update, patch, delete]
# Progressive syncs create Operations on behalf of the ApplicationSet.
- apiGroups: [cd.patrol.example]
  resources: [operations]
  verbs: [get, list, watch, create, delete, impersonate]
- apiGroups: [cd.patrol.example]
  resources: [scmproviders]
  verbs: [get, list, watch]
- apiGroups: [""]
  resources: [secrets, configmaps]
  verbs: [get, list]
- apiGroups: [""]
  resources: [events]
  verbs: [create, patch]
- apiGroups: [events.k8s.io]
  resources: [events]
  verbs: [create, patch]
{{- end -}}

{{/* Notifications controller, control-plane namespace. */}}
{{- define "patrol.rules.notifications.controlPlane" -}}
# Notification settings (list/watch field-selected by name) and the repository lister.
- apiGroups: [""]
  resources: [configmaps, secrets]
  verbs: [list, watch]
- apiGroups: [""]
  resources: [configmaps]
  resourceNames: [patrol-notifications-cm, patrol-cmd-params-cm]
  verbs: [get]
- apiGroups: [""]
  resources: [secrets]
  resourceNames: [patrol-notifications-secret]
  verbs: [get]
{{ include "patrol.rules.events" . }}
- apiGroups: [cd.patrol.example]
  resources: [appprojects, repositories, repositorycredentials, scmproviders]
  verbs: [get, list, watch]
# Leader election Lease patrol-notifications-controller.
- apiGroups: [coordination.k8s.io]
  resources: [leases]
  verbs: [create]
- apiGroups: [coordination.k8s.io]
  resources: [leases]
  resourceNames: [patrol-notifications-controller]
  verbs: [get, update]
{{ include "patrol.rules.notifications.applications" . }}
{{- end -}}

{{/*
"true" when the notifications controller runs with self-service notifications, resolved like
the binary (internal/server ParamFlags: flag > environment > patrol-cmd-params-cm; booleans
compared case-insensitively with "true"): notificationscontroller.selfservice.enabled in
configs.params, then a literal PATROL_NOTIFICATION_CONTROLLER_SELF_SERVICE_NOTIFICATION_ENABLED
in global.env or notificationsController.env (later entries win, as in the pod), then
--self-service-notification-enabled[=<bool>] in notificationsController.extraArgs (the last
one wins). Values from envFrom or valueFrom cannot be seen at render time.
*/}}
{{- define "patrol.notificationsSelfService" -}}
{{- $v := .Values -}}
{{- $on := eq (lower (toString (index $v.configs.params "notificationscontroller.selfservice.enabled" | default "false"))) "true" -}}
{{- range concat ($v.global.env | default list) ($v.notificationsController.env | default list) -}}
{{- if and (kindIs "map" .) (eq (toString .name) "PATROL_NOTIFICATION_CONTROLLER_SELF_SERVICE_NOTIFICATION_ENABLED") (hasKey . "value") -}}
{{- $on = eq (lower (toString .value)) "true" -}}
{{- end -}}
{{- end -}}
{{- range ($v.notificationsController.extraArgs | default list) -}}
{{- $a := toString . -}}
{{- if eq $a "--self-service-notification-enabled" -}}
{{- $on = true -}}
{{- else if hasPrefix "--self-service-notification-enabled=" $a -}}
{{- $on = eq (lower (trimPrefix "--self-service-notification-enabled=" $a)) "true" -}}
{{- end -}}
{{- end -}}
{{- if $on }}true{{ end -}}
{{- end -}}

{{/* Notifications controller, every Application namespace. */}}
{{- define "patrol.rules.notifications.applications" -}}
# Delivery state is stored as an annotation on the Application.
- apiGroups: [cd.patrol.example]
  resources: [applications]
  verbs: [get, list, watch, patch]
- apiGroups: [cd.patrol.example]
  resources: [operations]
  verbs: [get, list, watch]
{{- if include "patrol.notificationsSelfService" . }}
# Self-service notification configuration in Application namespaces: only the two named
# objects (the informers are field-selected by metadata.name, which RBAC authorizes against
# resourceNames), never other ConfigMaps or Secrets of those namespaces.
- apiGroups: [""]
  resources: [configmaps]
  resourceNames: [patrol-notifications-cm]
  verbs: [get, list, watch]
- apiGroups: [""]
  resources: [secrets]
  resourceNames: [patrol-notifications-secret]
  verbs: [get, list, watch]
{{- end }}
{{- end -}}

{{/*
First-run bootstrap (patrolcd bootstrap) in the init container of the primary workload, exactly
the contract of docs/architecture/components/bootstrap.md (TestBootstrapRBACContract): create the
Secrets when absent, read the three it owns, complete patrol-secret with full updates (no patch),
delete patrol-initial-admin-secret when it predates patrol-secret (a re-install after `helm
uninstall`, finding DV-6; the only delete it holds), read
patrol-cm (admin.enabled), and seed the default known hosts into the shipped
patrol-ssh-known-hosts-cm (only when it does). `create` cannot be restricted by name in
Kubernetes RBAC.
*/}}
{{- define "patrol.rules.bootstrap" -}}
- apiGroups: [""]
  resources: [secrets]
  verbs: [create]
- apiGroups: [""]
  resources: [secrets]
  resourceNames: [patrol-secret, patrol-internal-token, patrol-initial-admin-secret]
  verbs: [get]
- apiGroups: [""]
  resources: [secrets]
  resourceNames: [patrol-secret]
  verbs: [update]
- apiGroups: [""]
  resources: [secrets]
  resourceNames: [patrol-initial-admin-secret]
  verbs: [delete]
- apiGroups: [""]
  resources: [configmaps]
  resourceNames: [patrol-cm]
  verbs: [get]
{{- if include "patrol.seedsKnownHosts" . }}
- apiGroups: [""]
  resources: [configmaps]
  resourceNames: [patrol-ssh-known-hosts-cm]
  verbs: [get, update]
{{- end }}
{{- end -}}

{{/*
Role and RoleBinding. Usage:
include "patrol.roleBinding" (list $ name namespace component rulesYAML serviceAccountName)
*/}}
{{- define "patrol.roleBinding" -}}
{{- $ctx := index . 0 -}}
{{- $name := index . 1 -}}
{{- $ns := index . 2 -}}
{{- $component := index . 3 -}}
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: {{ $name }}
  namespace: {{ $ns }}
  labels:
    {{- include "patrol.labels" (list $ctx $component) | nindent 4 }}
  {{- with include "patrol.annotations" (list $ctx dict) }}
  {{- . | nindent 2 }}
  {{- end }}
rules:
  {{- index . 4 | nindent 2 }}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: {{ $name }}
  namespace: {{ $ns }}
  labels:
    {{- include "patrol.labels" (list $ctx $component) | nindent 4 }}
  {{- with include "patrol.annotations" (list $ctx dict) }}
  {{- . | nindent 2 }}
  {{- end }}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: {{ $name }}
subjects:
  - kind: ServiceAccount
    name: {{ index . 5 }}
    namespace: {{ $ctx.Release.Namespace }}
{{- end -}}

{{/*
ClusterRole and ClusterRoleBinding. Usage:
include "patrol.clusterRoleBinding" (list $ name component rulesYAML serviceAccountName)
*/}}
{{- define "patrol.clusterRoleBinding" -}}
{{- $ctx := index . 0 -}}
{{- $name := index . 1 -}}
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  {{- include "patrol.clusterMetadata" (list $ctx $name (index . 2)) | nindent 2 }}
rules:
  {{- index . 3 | nindent 2 }}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  {{- include "patrol.clusterMetadata" (list $ctx $name (index . 2)) | nindent 2 }}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: {{ $name }}
subjects:
  - kind: ServiceAccount
    name: {{ index . 4 }}
    namespace: {{ $ctx.Release.Namespace }}
{{- end -}}

{{/*
Namespaced rules for a list of extra namespaces: a Role per literal namespace, or one
ClusterRole when any entry is a glob or /regexp/ (apps in any namespace), or when the component
watches all namespaces as soon as any extra namespace is configured (clusterWide "true").
Usage: include "patrol.extraNamespaceRBAC" (list $ namespaces baseName component rulesYAML sa [clusterWide])
*/}}
{{- define "patrol.extraNamespaceRBAC" -}}
{{- $ctx := index . 0 -}}
{{- $nss := index . 1 -}}
{{- $base := index . 2 -}}
{{- $component := index . 3 -}}
{{- $rules := index . 4 -}}
{{- $sa := index . 5 -}}
{{- $clusterWide := and (gt (len .) 6) (index . 6) -}}
{{- if $nss -}}
{{- if and (not $clusterWide) (include "patrol.allLiteral" $nss) -}}
{{- range $i, $ns := (without (uniq $nss) $ctx.Release.Namespace) }}
{{- if $i }}
---
{{- end }}
{{ include "patrol.roleBinding" (list $ctx (include "patrol.name" (list $ctx $base)) $ns $component $rules $sa) }}
{{- end }}
{{- else -}}
{{ include "patrol.clusterRoleBinding" (list $ctx (include "patrol.clusterName" (list $ctx $base)) $component $rules $sa) }}
{{- end -}}
{{- end -}}
{{- end -}}
