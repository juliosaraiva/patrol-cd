{{/*
Naming, labels, images and shared fragments.

Object names: workloads, Services and ServiceAccounts are "<prefix>-<component>" (prefix
"patrol" unless fullnameOverride); settings objects keep the fixed names the binaries read;
cluster-scoped objects are "<release namespace>-<prefix>-<component>" so that installations in
different namespaces do not collide.

Convention: every fragment helper returns YAML without a leading or trailing newline, and ""
when there is nothing to render. Call sites indent with nindent, through `with` when the
fragment may be empty.
*/}}

{{/* Name prefix of workloads, Services and ServiceAccounts. */}}
{{- define "patrol.prefix" -}}
{{- default "patrol" .Values.fullnameOverride | trunc 40 | trimSuffix "-" -}}
{{- end -}}

{{/* Base of the app.kubernetes.io/name label. */}}
{{- define "patrol.nameBase" -}}
{{- default (include "patrol.prefix" .) .Values.nameOverride | trunc 40 | trimSuffix "-" -}}
{{- end -}}

{{/* "<prefix>-<component>". Usage: include "patrol.name" (list $ "repo-server") */}}
{{- define "patrol.name" -}}
{{- $ctx := index . 0 -}}
{{- printf "%s-%s" (include "patrol.prefix" $ctx) (index . 1) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
"<base>-<suffix>" within 63 characters (a DNS label, required for Service names): the base is
truncated before the suffix is added, so the suffix survives a long prefix. Names of objects
allowed 253 characters (ConfigMaps, Secrets) do not need it.
Usage: include "patrol.suffixedName" (list (include "patrol.serverName" .) "metrics")
*/}}
{{- define "patrol.suffixedName" -}}
{{- $suffix := index . 1 -}}
{{- printf "%s-%s" (index . 0 | trunc (sub 62 (len $suffix) | int) | trimSuffix "-") $suffix -}}
{{- end -}}

{{/* Name of a cluster-scoped object. Usage: include "patrol.clusterName" (list $ "controller") */}}
{{- define "patrol.clusterName" -}}
{{- $ctx := index . 0 -}}
{{- printf "%s-%s-%s" $ctx.Release.Namespace (include "patrol.prefix" $ctx) (index . 1) | trunc 253 | trimSuffix "-" -}}
{{- end -}}

{{- define "patrol.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* Selector labels. Usage: include "patrol.selectorLabels" (list $ "server") */}}
{{- define "patrol.selectorLabels" -}}
{{- $ctx := index . 0 -}}
app.kubernetes.io/name: {{ printf "%s-%s" (include "patrol.nameBase" $ctx) (index . 1) | trunc 63 | trimSuffix "-" }}
app.kubernetes.io/instance: {{ $ctx.Release.Name }}
{{- end -}}

{{/*
Labels of every object. app.kubernetes.io/part-of=patrol-cd is required: the settings manager
only reads ConfigMaps and referenced Secrets carrying it.
Usage: include "patrol.labels" (list $ "server")
*/}}
{{- define "patrol.labels" -}}
{{- $ctx := index . 0 -}}
{{ include "patrol.selectorLabels" . }}
app.kubernetes.io/component: {{ index . 1 }}
app.kubernetes.io/part-of: patrol-cd
app.kubernetes.io/managed-by: {{ $ctx.Release.Service }}
app.kubernetes.io/version: {{ $ctx.Chart.AppVersion | quote }}
helm.sh/chart: {{ include "patrol.chart" $ctx }}
{{- with $ctx.Values.global.commonLabels }}
{{ toYaml . }}
{{- end }}
{{- end -}}

{{/* Labels of the CRDs and the admission policy besides app.kubernetes.io/part-of (already in the source). */}}
{{- define "patrol.crdLabels" -}}
{{ include "patrol.selectorLabels" (list . "crds") }}
app.kubernetes.io/component: crds
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
helm.sh/chart: {{ include "patrol.chart" . }}
{{- with .Values.global.commonLabels }}
{{ toYaml . }}
{{- end }}
{{- end -}}

{{/*
Object labels with extra labels. Usage: include "patrol.labelsWith" (list $ "server" .Values.server.labels)
*/}}
{{- define "patrol.labelsWith" -}}
{{ include "patrol.labels" (list (index . 0) (index . 1)) }}
{{- with (index . 2) }}
{{ toYaml . }}
{{- end }}
{{- end -}}

{{/*
"annotations:" block of an object (global commonAnnotations plus extra), or "".
Usage: include "patrol.annotations" (list $ .Values.server.annotations)
*/}}
{{- define "patrol.annotations" -}}
{{- $ctx := index . 0 -}}
{{- $all := merge (deepCopy (index . 1 | default dict)) (deepCopy ($ctx.Values.global.commonAnnotations | default dict)) -}}
{{- if $all -}}
annotations:
{{ toYaml $all | indent 2 }}
{{- end -}}
{{- end -}}

{{/* Standard metadata of a namespaced object. Usage: include "patrol.metadata" (list $ "name" "component" labels annotations) */}}
{{- define "patrol.metadata" -}}
{{- $ctx := index . 0 -}}
name: {{ index . 1 }}
namespace: {{ $ctx.Release.Namespace }}
labels:
{{ include "patrol.labelsWith" (list $ctx (index . 2) (index . 3)) | indent 2 }}
{{- with (include "patrol.annotations" (list $ctx (index . 4))) }}
{{ . }}
{{- end }}
{{- end -}}

{{/* Standard metadata of a cluster-scoped object. Usage: include "patrol.clusterMetadata" (list $ "name" "component") */}}
{{- define "patrol.clusterMetadata" -}}
{{- $ctx := index . 0 -}}
name: {{ index . 1 }}
labels:
{{ include "patrol.labels" (list $ctx (index . 2)) | indent 2 }}
{{- with (include "patrol.annotations" (list $ctx dict)) }}
{{ . }}
{{- end }}
{{- end -}}

{{/*
Image reference. global.imageRegistry replaces the image's own registry (air-gapped mirrors).
Usage: include "patrol.image" (list $ .Values.image $.Chart.AppVersion)
*/}}
{{- define "patrol.image" -}}
{{- $ctx := index . 0 -}}
{{- $img := index . 1 -}}
{{- $defaultTag := index . 2 -}}
{{- $registry := default $img.registry $ctx.Values.global.imageRegistry -}}
{{- $ref := $img.repository -}}
{{- if $registry }}{{ $ref = printf "%s/%s" (trimSuffix "/" $registry) $img.repository }}{{ end -}}
{{- $tag := default $defaultTag (toString ($img.tag | default "")) -}}
{{- if $tag }}{{ $ref = printf "%s:%s" $ref $tag }}{{ end -}}
{{- if $img.digest }}{{ $ref = printf "%s@%s" $ref $img.digest }}{{ end -}}
{{- $ref -}}
{{- end -}}

{{/* The Patrol image. */}}
{{- define "patrol.patrolImage" -}}
{{- include "patrol.image" (list . .Values.image .Chart.AppVersion) -}}
{{- end -}}

{{/* "imagePullSecrets:" block of a pod spec, or "". */}}
{{- define "patrol.imagePullSecrets" -}}
{{- $names := list -}}
{{- range .Values.global.imagePullSecrets -}}
{{- if kindIs "map" . -}}
{{- $names = append $names (dict "name" .name) -}}
{{- else -}}
{{- $names = append $names (dict "name" .) -}}
{{- end -}}
{{- end -}}
{{- with $names -}}
imagePullSecrets:
{{ toYaml . | indent 2 }}
{{- end -}}
{{- end -}}

{{/*
Whether an API version is available: the cluster's capabilities or capabilities.extraAPIVersions.
Usage: include "patrol.hasAPI" (list $ "cert-manager.io/v1") -> "true" or ""
*/}}
{{- define "patrol.hasAPI" -}}
{{- $ctx := index . 0 -}}
{{- $api := index . 1 -}}
{{- if or ($ctx.Capabilities.APIVersions.Has $api) (has $api $ctx.Values.capabilities.extraAPIVersions) -}}true{{- end -}}
{{- end -}}

{{/*
ServiceAccount name: serviceAccount.name, else "<prefix>-<component>" when the chart creates it.
With create=false the name is required (patrol.validate): the chart never falls back to the
namespace's "default" ServiceAccount, which every pod without serviceAccountName shares.
Usage: include "patrol.serviceAccountName" (list $ .Values.server "server")
*/}}
{{- define "patrol.serviceAccountName" -}}
{{- $ctx := index . 0 -}}
{{- $v := index . 1 -}}
{{- if $v.serviceAccount.create -}}
{{- default (include "patrol.name" (list $ctx (index . 2))) $v.serviceAccount.name -}}
{{- else -}}
{{- $v.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/* Component names used in object names. */}}
{{- define "patrol.serverName" -}}{{ include "patrol.name" (list . "server") }}{{- end -}}
{{- define "patrol.controllerName" -}}{{ include "patrol.name" (list . "controller") }}{{- end -}}
{{- define "patrol.repoServerName" -}}{{ include "patrol.name" (list . "repo-server") }}{{- end -}}
{{- define "patrol.appsetName" -}}{{ include "patrol.name" (list . "applicationset-controller") }}{{- end -}}
{{- define "patrol.notificationsName" -}}{{ include "patrol.name" (list . "notifications-controller") }}{{- end -}}
{{- define "patrol.commitServerName" -}}{{ include "patrol.name" (list . "commit-server") }}{{- end -}}
{{- define "patrol.dexName" -}}{{ include "patrol.name" (list . "dex-server") }}{{- end -}}

{{/* The workload running the bootstrap init container: server, else controller. */}}
{{- define "patrol.primary" -}}
{{- if .Values.server.enabled -}}server{{- else if .Values.controller.enabled -}}controller{{- end -}}
{{- end -}}

{{/* Whether the commit-server is deployed. */}}
{{- define "patrol.commitServerEnabled" -}}
{{- if and .Values.hydrator.enabled .Values.controller.enabled -}}true{{- end -}}
{{- end -}}

{{/* Name of the Secret holding the internal token. */}}
{{- define "patrol.internalTokenSecret" -}}
{{- default "patrol-internal-token" .Values.configs.internalToken.existingSecret -}}
{{- end -}}

{{/*
Internal TLS predicates ("true" or "").
- repoTLS: the repo-server serves TLS (every mode but disabled).
- managed: certificates come from Secrets (certManager or existingSecret).
- verify / mtls: managed and the corresponding switch.
*/}}
{{- define "patrol.tls.repoTLS" -}}
{{- if ne .Values.internalTLS.mode "disabled" -}}true{{- end -}}
{{- end -}}
{{- define "patrol.tls.managed" -}}
{{- if has .Values.internalTLS.mode (list "certManager" "existingSecret") -}}true{{- end -}}
{{- end -}}
{{- define "patrol.tls.verify" -}}
{{- if and (include "patrol.tls.managed" .) .Values.internalTLS.verify -}}true{{- end -}}
{{- end -}}
{{- define "patrol.tls.mtls" -}}
{{- if and (include "patrol.tls.managed" .) .Values.internalTLS.mtls -}}true{{- end -}}
{{- end -}}

{{/* Addresses wired into patrol-cmd-params-cm and flags. */}}
{{- define "patrol.repoServerAddress" -}}
{{- printf "%s:8081" (include "patrol.repoServerName" .) -}}
{{- end -}}
{{- define "patrol.commitServerURL" -}}
{{- if include "patrol.tls.managed" . -}}
{{- printf "https://%s:8086" (include "patrol.commitServerName" .) -}}
{{- else -}}
{{- printf "%s:8086" (include "patrol.commitServerName" .) -}}
{{- end -}}
{{- end -}}

{{/* "true" when every namespace is a literal name (no glob/regexp): namespaced RBAC suffices. */}}
{{- define "patrol.allLiteral" -}}
{{- $ok := true -}}
{{- range . -}}
{{- if not (regexMatch "^[a-z0-9]([-a-z0-9]*[a-z0-9])?$" (. | toString | trim)) -}}{{- $ok = false -}}{{- end -}}
{{- end -}}
{{- if $ok -}}true{{- end -}}
{{- end -}}

{{/* Value rendered as a ConfigMap string: booleans as true/false, whole numbers without exponent. */}}
{{- define "patrol.stringify" -}}
{{- $v := . -}}
{{- if kindIs "float64" $v -}}
{{- if eq ($v | int64 | float64) $v -}}{{- $v | int64 | toString -}}{{- else -}}{{- $v | toString -}}{{- end -}}
{{- else if kindIs "invalid" $v -}}
{{- else -}}
{{- $v | toString -}}
{{- end -}}
{{- end -}}

{{/* Pod security context of a component. Usage: include "patrol.podSecurityContext" (list $ .Values.server) */}}
{{- define "patrol.podSecurityContext" -}}
{{- $ctx := index . 0 -}}
{{- $v := index . 1 -}}
{{- toYaml (mergeOverwrite (deepCopy $ctx.Values.global.podSecurityContext) (deepCopy ($v.podSecurityContext | default dict))) -}}
{{- end -}}

{{/* Container security context. Usage: include "patrol.containerSecurityContext" (list $ .Values.server) */}}
{{- define "patrol.containerSecurityContext" -}}
{{- $ctx := index . 0 -}}
{{- $v := index . 1 -}}
{{- toYaml (mergeOverwrite (deepCopy $ctx.Values.global.containerSecurityContext) (deepCopy ($v.containerSecurityContext | default dict))) -}}
{{- end -}}

{{/*
Scheduling fields of a pod spec: nodeSelector, tolerations, affinity (or the anti-affinity
preset), topologySpreadConstraints (or the zone spread preset), priorityClassName.
Usage: include "patrol.scheduling" (list $ .Values.server "server")
*/}}
{{- define "patrol.scheduling" -}}
{{- $ctx := index . 0 -}}
{{- $v := index . 1 -}}
{{- $component := index . 2 -}}
{{- $g := $ctx.Values.global -}}
{{- $out := dict -}}
{{- $selector := include "patrol.selectorLabels" (list $ctx $component) | fromYaml -}}
{{- with ($v.nodeSelector | default $g.nodeSelector) }}{{ $_ := set $out "nodeSelector" . }}{{ end -}}
{{- with ($v.tolerations | default $g.tolerations) }}{{ $_ := set $out "tolerations" . }}{{ end -}}
{{- if $v.affinity -}}
{{- $_ := set $out "affinity" $v.affinity -}}
{{- else if eq $g.podAntiAffinity "hard" -}}
{{- $term := dict "topologyKey" $g.podAntiAffinityTopologyKey "labelSelector" (dict "matchLabels" $selector) -}}
{{- $_ := set $out "affinity" (dict "podAntiAffinity" (dict "requiredDuringSchedulingIgnoredDuringExecution" (list $term))) -}}
{{- else if eq $g.podAntiAffinity "soft" -}}
{{- $term := dict "topologyKey" $g.podAntiAffinityTopologyKey "labelSelector" (dict "matchLabels" $selector) -}}
{{- $_ := set $out "affinity" (dict "podAntiAffinity" (dict "preferredDuringSchedulingIgnoredDuringExecution" (list (dict "weight" 100 "podAffinityTerm" $term)))) -}}
{{- end -}}
{{- if $v.topologySpreadConstraints -}}
{{- $_ := set $out "topologySpreadConstraints" $v.topologySpreadConstraints -}}
{{- else if $g.topologySpread -}}
{{- $c := dict "maxSkew" 1 "topologyKey" $g.topologySpreadTopologyKey "whenUnsatisfiable" (ternary "DoNotSchedule" "ScheduleAnyway" (eq $g.topologySpread "hard")) "labelSelector" (dict "matchLabels" $selector) -}}
{{- $_ := set $out "topologySpreadConstraints" (list $c) -}}
{{- end -}}
{{- with ($v.priorityClassName | default $g.priorityClassName) }}{{ $_ := set $out "priorityClassName" . }}{{ end -}}
{{- if $out }}{{ toYaml $out }}{{ end -}}
{{- end -}}

{{/* Pod labels (labels + global and component pod labels). Usage: include "patrol.podLabels" (list $ .Values.server "server") */}}
{{- define "patrol.podLabels" -}}
{{- $ctx := index . 0 -}}
{{- $v := index . 1 -}}
{{ include "patrol.labels" (list $ctx (index . 2)) }}
{{- with $ctx.Values.global.podLabels }}
{{ toYaml . }}
{{- end }}
{{- with $v.podLabels }}
{{ toYaml . }}
{{- end }}
{{- end -}}

{{/*
Pod annotations: checksums plus global and component annotations.
Usage: include "patrol.podAnnotations" (list $ .Values.server (dict "checksum/cmd-params" "..."))
*/}}
{{- define "patrol.podAnnotations" -}}
{{- $ctx := index . 0 -}}
{{- $v := index . 1 -}}
{{- $all := merge (deepCopy (index . 2 | default dict)) (deepCopy ($v.podAnnotations | default dict)) (deepCopy ($ctx.Values.global.podAnnotations | default dict)) -}}
{{- if $all }}{{ toYaml $all }}{{ end -}}
{{- end -}}

{{/*
Environment shared by every Patrol container: POD_NAMESPACE, GOMEMLIMIT (from the memory
limit), global and component variables. Usage: include "patrol.commonEnv" (list $ .Values.server)
*/}}
{{- define "patrol.commonEnv" -}}
{{- $ctx := index . 0 -}}
{{- $v := index . 1 -}}
- name: POD_NAMESPACE
  valueFrom:
    fieldRef:
      fieldPath: metadata.namespace
{{- if and $ctx.Values.global.goMemLimitFromResources (dig "limits" "memory" "" ($v.resources | default dict)) }}
- name: GOMEMLIMIT
  valueFrom:
    resourceFieldRef:
      resource: limits.memory
{{- end }}
{{- with $ctx.Values.global.env }}
{{ toYaml . }}
{{- end }}
{{- with $v.env }}
{{ toYaml . }}
{{- end }}
{{- end -}}

{{/* "envFrom:" block of a Patrol container, or "". */}}
{{- define "patrol.envFrom" -}}
{{- $ctx := index . 0 -}}
{{- $v := index . 1 -}}
{{- with concat ($ctx.Values.global.envFrom | default list) ($v.envFrom | default list) -}}
envFrom:
{{ toYaml . | indent 2 }}
{{- end -}}
{{- end -}}

{{/*
Environment variables projecting patrol-cmd-params-cm into a component without Kubernetes access
(repo-server, commit-server), from the generated parameter table.
Usage: include "patrol.paramEnv" (list $ "repo-server")
*/}}
{{- define "patrol.paramEnv" -}}
{{- $ctx := index . 0 -}}
{{- $table := include "patrol.generated.params" $ctx | fromYaml -}}
{{- $env := list -}}
{{- range (index $table.env (index . 1)) -}}
{{- $env = append $env (dict "name" .env "valueFrom" (dict "configMapKeyRef" (dict "name" "patrol-cmd-params-cm" "key" .key "optional" true))) -}}
{{- end -}}
{{- if $env }}{{ toYaml $env }}{{ end -}}
{{- end -}}

{{/* OTLP headers from a Secret (never written to the ConfigMap), or "". Usage: include "patrol.otlpHeadersEnv" (list $ "PATROL_SERVER_OTLP_HEADERS") */}}
{{- define "patrol.otlpHeadersEnv" -}}
{{- $ctx := index . 0 -}}
{{- $s := $ctx.Values.tracing.headersSecret -}}
{{- if $s.name -}}
- name: {{ index . 1 }}
  valueFrom:
    secretKeyRef:
      name: {{ $s.name }}
      key: {{ $s.key }}
{{- end -}}
{{- end -}}

{{/*
Profiler toggle file volume: the <component>.profile.enabled key projected to
/home/patrol/params/profiler.enabled (missing key: profiler disabled), or "".
Usage: include "patrol.profileVolume" (list $ "server")
*/}}
{{- define "patrol.profileVolume" -}}
{{- $ctx := index . 0 -}}
{{- $table := include "patrol.generated.params" $ctx | fromYaml -}}
{{- with (index $table.profile (index . 1)) -}}
- name: params
  configMap:
    name: patrol-cmd-params-cm
    optional: true
    items:
      - key: {{ . }}
        path: profiler.enabled
{{- end -}}
{{- end -}}

{{/* Mount of the profiler toggle volume, or "". Usage: include "patrol.profileMount" (list $ "server") */}}
{{- define "patrol.profileMount" -}}
{{- if include "patrol.profileVolume" . -}}
- name: params
  mountPath: /home/patrol/params
  readOnly: true
{{- end -}}
{{- end -}}

{{/*
Internal token volume. optional is true only for the primary workload while the bootstrap step
creates the Secret: the pod must start to run its init container, and the binary falls back to
reading the Secret through the API when the file is missing. Other workloads wait for the Secret.
Usage: include "patrol.internalTokenVolume" (list $ true)
*/}}
{{- define "patrol.internalTokenVolume" -}}
{{- $ctx := index . 0 -}}
{{- $primary := index . 1 -}}
{{- $t := $ctx.Values.configs.internalToken -}}
- name: internal-token
  secret:
    secretName: {{ include "patrol.internalTokenSecret" $ctx }}
    defaultMode: 0440
    items:
      - key: {{ ternary $t.existingSecretKey "token" (ne $t.existingSecret "") }}
        path: token
    {{- if and $primary $ctx.Values.bootstrap.enabled (not $t.existingSecret) }}
    optional: true
    {{- end }}
{{- end -}}

{{/* Mount of the internal token volume. */}}
{{- define "patrol.internalTokenMount" -}}
- name: internal-token
  mountPath: /app/config/internal-token
  readOnly: true
{{- end -}}

{{/*
Volumes of an internal API client: the mTLS client pair and the CA bundles verifying internal
certificates, or "". Usage: include "patrol.internalClientVolumes" (list $ (dict "repo" true))
*/}}
{{- define "patrol.internalClientVolumes" -}}
{{- $ctx := index . 0 -}}
{{- $want := index . 1 -}}
{{- $vols := list -}}
{{- if include "patrol.tls.mtls" $ctx -}}
{{- $vols = append $vols (dict "name" "internal-client-tls" "secret" (dict "secretName" "patrol-internal-client-tls" "defaultMode" 288 "items" (list (dict "key" "tls.crt" "path" "client.crt") (dict "key" "tls.key" "path" "client.key")))) -}}
{{- end -}}
{{- if include "patrol.tls.verify" $ctx -}}
{{- $sources := list -}}
{{- if $want.repo }}{{ $sources = append $sources (dict "secret" (dict "name" "patrol-repo-server-tls" "items" (list (dict "key" "ca.crt" "path" "repo-server/ca.crt")))) }}{{ end -}}
{{- if $want.commit }}{{ $sources = append $sources (dict "secret" (dict "name" "patrol-commit-server-tls" "items" (list (dict "key" "ca.crt" "path" "commit-server/ca.crt")))) }}{{ end -}}
{{- if $want.controller }}{{ $sources = append $sources (dict "secret" (dict "name" "patrol-controller-tls" "items" (list (dict "key" "ca.crt" "path" "controller/ca.crt")))) }}{{ end -}}
{{- if $sources -}}
{{- $vols = append $vols (dict "name" "internal-ca" "projected" (dict "defaultMode" 292 "sources" $sources)) -}}
{{- end -}}
{{- end -}}
{{- if $vols }}{{ toYaml $vols }}{{ end -}}
{{- end -}}

{{/* Mounts matching patrol.internalClientVolumes, or "". */}}
{{- define "patrol.internalClientMounts" -}}
{{- $ctx := index . 0 -}}
{{- $want := index . 1 -}}
{{- $mounts := list -}}
{{- if include "patrol.tls.mtls" $ctx -}}
{{- $mounts = append $mounts (dict "name" "internal-client-tls" "mountPath" "/app/config/reposerver/mtls" "readOnly" true) -}}
{{- end -}}
{{- if and (include "patrol.tls.verify" $ctx) (or $want.repo $want.commit $want.controller) -}}
{{- $mounts = append $mounts (dict "name" "internal-ca" "mountPath" "/app/config/internal-tls" "readOnly" true) -}}
{{- end -}}
{{- if $mounts }}{{ toYaml $mounts }}{{ end -}}
{{- end -}}

{{/* Repo-server client flags (server, controller, appset, notifications), or "". */}}
{{- define "patrol.repoClientArgs" -}}
{{- if not (include "patrol.tls.repoTLS" .) -}}
- --repo-server-plaintext
{{- else if include "patrol.tls.verify" . -}}
- --repo-server-ca-cert-path=/app/config/internal-tls/repo-server/ca.crt
{{- end -}}
{{- end -}}

{{/* Checksum of the rendered patrol-cmd-params-cm (read at startup, so a change rolls pods). */}}
{{- define "patrol.paramsChecksum" -}}
{{- include (print .Template.BasePath "/config/cmd-params-cm.yaml") . | sha256sum -}}
{{- end -}}

{{/* Probe. Usage: include "patrol.probe" (list .Values.server.probes.readiness "/readyz" "metrics" "HTTP") */}}
{{- define "patrol.probe" -}}
{{- $p := index . 0 -}}
httpGet:
  path: {{ index . 1 }}
  port: {{ index . 2 }}
  scheme: {{ index . 3 | default "HTTP" }}
{{- with $p.initialDelaySeconds }}
initialDelaySeconds: {{ . }}
{{- end }}
periodSeconds: {{ $p.periodSeconds }}
timeoutSeconds: {{ $p.timeoutSeconds }}
failureThreshold: {{ $p.failureThreshold }}
{{- end -}}

{{/*
PodDisruptionBudget availability field: minAvailable when set, else maxUnavailable (an explicit
0 is kept: it blocks voluntary evictions), else 1. Usage: include "patrol.pdbSpec" .Values.server.pdb
*/}}
{{- define "patrol.pdbSpec" -}}
{{- if and (hasKey . "minAvailable") (ne (toString .minAvailable) "") -}}
minAvailable: {{ .minAvailable }}
{{- else if and (hasKey . "maxUnavailable") (ne (toString .maxUnavailable) "") -}}
maxUnavailable: {{ .maxUnavailable }}
{{- else -}}
maxUnavailable: 1
{{- end -}}
{{- end -}}

{{/* --shutdown-timeout flag. */}}
{{- define "patrol.shutdownArg" -}}
- --shutdown-timeout={{ .Values.global.shutdownTimeoutSeconds }}s
{{- end -}}

{{/* tmp emptyDir volume. Usage: include "patrol.tmpVolume" "1Gi" */}}
{{- define "patrol.tmpVolume" -}}
- name: tmp
  emptyDir:
    sizeLimit: {{ . }}
{{- end -}}
