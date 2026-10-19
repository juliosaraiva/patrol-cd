{{/*
The bootstrap init container (`patrolcd bootstrap`), added to the primary workload. It uses the
pod's ServiceAccount (an init container cannot have its own); the chart binds the narrow
<prefix>-bootstrap Role to that ServiceAccount (templates/rbac/bootstrap.yaml).
*/}}

{{/*
"true" when the bootstrap seeds the default SSH known hosts: the chart renders
patrol-ssh-known-hosts-cm without ssh_known_hosts (configs.sshKnownHosts empty).
*/}}
{{- define "patrol.seedsKnownHosts" -}}
{{- if and .Values.configs.create.sshKnownHosts (not .Values.configs.sshKnownHosts) -}}true{{- end -}}
{{- end -}}

{{/*
"true" unless configs.cm disables the admin account: admin.enabled and accounts.admin.enabled,
when set, must be a true boolean (strconv.ParseBool, as internal/auth; any false or invalid value
disables the account and the bootstrap then generates no password).
*/}}
{{- define "patrol.adminEnabled" -}}
{{- $on := true -}}
{{- range (list "admin.enabled" "accounts.admin.enabled") -}}
{{- if hasKey $.Values.configs.cm . -}}
{{- if not (has (trim (toString (index $.Values.configs.cm .))) (list "1" "t" "T" "TRUE" "true" "True")) -}}
{{- $on = false -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- if $on }}true{{ end -}}
{{- end -}}

{{/* "true" when the component runs the bootstrap init container. Usage: include "patrol.runsBootstrap" (list $ "server") */}}
{{- define "patrol.runsBootstrap" -}}
{{- $ctx := index . 0 -}}
{{- if and $ctx.Values.bootstrap.enabled (eq (include "patrol.primary" $ctx) (index . 1)) -}}true{{- end -}}
{{- end -}}

{{/*
initContainers list of a Patrol workload: bootstrap (primary only) and the component's extra
init containers, or "". Usage: include "patrol.initContainers" (list $ .Values.server "server")
*/}}
{{- define "patrol.initContainers" -}}
{{- $ctx := index . 0 -}}
{{- $c := index . 1 -}}
{{- $v := $ctx.Values -}}
{{- if or (include "patrol.runsBootstrap" (list $ctx (index . 2))) $c.initContainers -}}
initContainers:
{{- if include "patrol.runsBootstrap" (list $ctx (index . 2)) }}
  - name: bootstrap
    image: {{ include "patrol.patrolImage" $ctx }}
    imagePullPolicy: {{ $v.image.pullPolicy }}
    command:
      - /usr/local/bin/patrolcd
    args:
      - bootstrap
      - --timeout={{ $v.bootstrap.timeoutSeconds }}s
      {{- if or (not $v.configs.secret.createAdmin) $v.configs.secret.adminPasswordHash }}
      - --skip-admin
      {{- end }}
      {{- if $v.configs.internalToken.existingSecret }}
      - --skip-internal-token
      {{- end }}
      {{- if not (include "patrol.seedsKnownHosts" $ctx) }}
      - --skip-ssh-known-hosts
      {{- end }}
      {{- if has $v.global.logLevel (list "debug" "info" "warn" "error") }}
      - --loglevel={{ $v.global.logLevel }}
      {{- end }}
      {{- with $v.global.logFormat }}
      - --logformat={{ . }}
      {{- end }}
    env:
      - name: POD_NAMESPACE
        valueFrom:
          fieldRef:
            fieldPath: metadata.namespace
    securityContext:
      {{- include "patrol.containerSecurityContext" (list $ctx $c) | nindent 6 }}
    resources:
      {{- toYaml $v.bootstrap.resources | nindent 6 }}
{{- end }}
{{- with $c.initContainers }}
{{ toYaml . | indent 2 }}
{{- end }}
{{- end -}}
{{- end -}}
