{{/*
patrol-cmd-params-cm data and value validation.
*/}}

{{/*
Parameters driven by structured values. Setting them in configs.params fails rendering, because
the chart passes the corresponding flags (which win over the ConfigMap) or computes them.
*/}}
{{- define "patrol.managedParams" -}}
- application.namespaces
- applicationsetcontroller.namespaces
- hydrator.enabled
- commit.server
- reposerver.disable.tls
- reposerver.client.ca.path
- server.repo.server.plaintext
- server.repo.server.ca.cert.path
- server.repo.server.client.cert.path
- server.repo.server.client.cert.key.path
- controller.repo.server.plaintext
- controller.repo.server.ca.cert.path
- controller.repo.server.client.cert.path
- controller.repo.server.client.cert.key.path
- applicationsetcontroller.repo.server.plaintext
- applicationsetcontroller.repo.server.ca.cert.path
- applicationsetcontroller.repo.server.client.cert.path
- applicationsetcontroller.repo.server.client.cert.key.path
- applicationsetcontroller.enable.leader.election
- notificationscontroller.repo.server.plaintext
- notificationscontroller.repo.server.ca.cert.path
- notificationscontroller.repo.server.client.cert.path
- notificationscontroller.repo.server.client.cert.key.path
- server.insecure
{{- end -}}

{{/* Component prefixes of the <prefix>.log.level / .log.format parameters. */}}
{{- define "patrol.logPrefixes" -}}
- server
- controller
- reposerver
- applicationsetcontroller
- notificationscontroller
- commitserver
{{- end -}}

{{/*
patrol-cmd-params-cm data: values computed from the structured settings, then configs.params
(which wins for every key that is not managed).
*/}}
{{- define "patrol.paramsData" -}}
{{- $v := .Values -}}
{{- $p := dict -}}
{{- if $v.repoServer.enabled -}}
{{- $_ := set $p "repo.server" (include "patrol.repoServerAddress" .) -}}
{{- end -}}
{{- if $v.applicationNamespaces -}}
{{- $_ := set $p "application.namespaces" (join "," $v.applicationNamespaces) -}}
{{- end -}}
{{- if $v.applicationSetNamespaces -}}
{{- $_ := set $p "applicationsetcontroller.namespaces" (join "," $v.applicationSetNamespaces) -}}
{{- end -}}
{{- if $v.hydrator.enabled -}}
{{- $_ := set $p "hydrator.enabled" "true" -}}
{{- $_ := set $p "commit.server" (include "patrol.commitServerURL" .) -}}
{{- end -}}
{{- if $v.dex.enabled -}}
{{- $_ := set $p "server.dex.server" (printf "%s:5556" (include "patrol.dexName" .)) -}}
{{- $_ := set $p "server.dex.server.plaintext" "true" -}}
{{- end -}}
{{- if $v.controller.sharding.algorithm -}}
{{- $_ := set $p "controller.sharding.algorithm" $v.controller.sharding.algorithm -}}
{{- end -}}
{{- range (include "patrol.logPrefixes" . | fromYamlArray) -}}
{{- if $v.global.logLevel }}{{ $_ := set $p (printf "%s.log.level" .) $v.global.logLevel }}{{ end -}}
{{- if $v.global.logFormat }}{{ $_ := set $p (printf "%s.log.format" .) $v.global.logFormat }}{{ end -}}
{{- end -}}
{{- with $v.tracing.otlpAddress -}}
{{- $_ := set $p "otlp.address" . -}}
{{- $_ := set $p "otlp.insecure" (toString $v.tracing.insecure) -}}
{{- with $v.tracing.attrs }}{{ $_ := set $p "otlp.attrs" (join "," .) }}{{ end -}}
{{- end -}}
{{- range $k, $val := $v.configs.params -}}
{{- $_ := set $p $k (include "patrol.stringify" $val) -}}
{{- end -}}
{{- toYaml $p -}}
{{- end -}}

{{/*
Validation of combinations the schema cannot express. Rendered once (from the cmd-params
ConfigMap template, which always renders).
*/}}
{{- define "patrol.validate" -}}
{{- $v := .Values -}}
{{- $managed := include "patrol.managedParams" . | fromYamlArray -}}
{{- range $k, $_ := $v.configs.params -}}
{{- if has $k $managed -}}
{{- fail (printf "configs.params.%s is managed by the chart (structured values and internalTLS); remove it" $k) -}}
{{- end -}}
{{- if and $v.dex.enabled (has $k (list "server.dex.server" "server.dex.server.plaintext" "server.dex.server.strict.tls")) -}}
{{- fail (printf "configs.params.%s cannot be set together with dex.enabled" $k) -}}
{{- end -}}
{{- if and $v.global.logLevel (hasSuffix ".log.level" $k) -}}
{{- fail (printf "configs.params.%s conflicts with global.logLevel; set one of them" $k) -}}
{{- end -}}
{{- if and $v.global.logFormat (hasSuffix ".log.format" $k) -}}
{{- fail (printf "configs.params.%s conflicts with global.logFormat; set one of them" $k) -}}
{{- end -}}
{{- if and $v.tracing.otlpAddress (hasPrefix "otlp." $k) -}}
{{- fail (printf "configs.params.%s conflicts with tracing.otlpAddress; set one of them" $k) -}}
{{- end -}}
{{- if and $v.controller.sharding.algorithm (eq $k "controller.sharding.algorithm") -}}
{{- fail "configs.params.controller.sharding.algorithm conflicts with controller.sharding.algorithm; set one of them" -}}
{{- end -}}
{{- if eq $k "otlp.headers" -}}
{{- fail "configs.params.otlp.headers would put OTLP headers into a ConfigMap; use tracing.headersSecret" -}}
{{- end -}}
{{- end -}}
{{- if not (or $v.server.enabled $v.controller.enabled) -}}
{{- fail "at least one of server.enabled and controller.enabled must be true" -}}
{{- end -}}
{{- if and $v.dex.enabled (not $v.server.enabled) -}}
{{- fail "dex.enabled requires server.enabled (patrol-server proxies /api/dex)" -}}
{{- end -}}
{{- if and $v.dex.enabled (not $v.dex.existingConfigSecret) -}}
{{- if not $v.dex.clientSecretRef.name -}}
{{- fail "dex.enabled needs dex.clientSecretRef.name (client secret of the patrol static client) or dex.existingConfigSecret" -}}
{{- end -}}
{{- if not (index $v.configs.cm "url") -}}
{{- fail "dex.enabled needs configs.cm.url (the Dex issuer is <url>/api/dex)" -}}
{{- end -}}
{{- end -}}
{{- if and $v.hydrator.enabled (not $v.controller.enabled) -}}
{{- fail "hydrator.enabled requires controller.enabled" -}}
{{- end -}}
{{- if eq $v.rbac.scope "namespaced" -}}
{{- if or $v.applicationNamespaces $v.applicationSetNamespaces -}}
{{- fail "rbac.scope=namespaced cannot be combined with applicationNamespaces or applicationSetNamespaces (namespace-install semantics)" -}}
{{- end -}}
{{- end -}}
{{- /* The ApplicationSet controller refuses to start with ApplicationSets in other namespaces,
SCM providers enabled and no allow-list (internal/appset ErrSCMProvidersRequireAllowList). */ -}}
{{- if and $v.appsetController.enabled (without $v.applicationSetNamespaces .Release.Namespace) -}}
{{- $scm := "true" -}}
{{- if hasKey $v.configs.params "applicationsetcontroller.enable.scm.providers" -}}
{{- $scm = toString (index $v.configs.params "applicationsetcontroller.enable.scm.providers") -}}
{{- end -}}
{{- $args := join " " $v.appsetController.extraArgs -}}
{{- if and (ne $scm "false") (not (index $v.configs.params "applicationsetcontroller.allowed.scm.providers")) (not (contains "--allowed-scm-providers" $args)) (not (contains "--enable-scm-providers=false" $args)) -}}
{{- fail "applicationSetNamespaces needs configs.params applicationsetcontroller.allowed.scm.providers (or applicationsetcontroller.enable.scm.providers: false): the ApplicationSet controller refuses to start otherwise" -}}
{{- end -}}
{{- end -}}
{{- if and (eq $v.internalTLS.mode "certManager") (not (include "patrol.hasAPI" (list . "cert-manager.io/v1"))) -}}
{{- fail "internalTLS.mode=certManager needs cert-manager (cert-manager.io/v1); install it, or add it to capabilities.extraAPIVersions when rendering offline" -}}
{{- end -}}
{{- if and (not (include "patrol.tls.managed" .)) $v.internalTLS.mtls -}}
{{- fail "internalTLS.mtls needs internalTLS.mode certManager or existingSecret" -}}
{{- end -}}
{{- if $v.server.certificate.enabled -}}
{{- if not (include "patrol.hasAPI" (list . "cert-manager.io/v1")) -}}
{{- fail "server.certificate.enabled needs cert-manager (cert-manager.io/v1)" -}}
{{- end -}}
{{- if not $v.server.certificate.issuerRef.name -}}
{{- fail "server.certificate.issuerRef.name is required" -}}
{{- end -}}
{{- if not $v.server.certificate.dnsNames -}}
{{- fail "server.certificate.dnsNames is required" -}}
{{- end -}}
{{- if $v.server.tls.existingSecret -}}
{{- fail "set either server.tls.existingSecret or server.certificate.enabled" -}}
{{- end -}}
{{- end -}}
{{- if and $v.server.httproute.enabled (not (include "patrol.hasAPI" (list . "gateway.networking.k8s.io/v1"))) -}}
{{- fail "server.httproute.enabled needs the Gateway API (gateway.networking.k8s.io/v1)" -}}
{{- end -}}
{{- if or $v.metrics.serviceMonitor.enabled $v.metrics.prometheusRule.enabled -}}
{{- if not (include "patrol.hasAPI" (list . "monitoring.coreos.com/v1")) -}}
{{- fail "metrics.serviceMonitor/prometheusRule need the Prometheus Operator CRDs (monitoring.coreos.com/v1)" -}}
{{- end -}}
{{- end -}}
{{- if and $v.configs.secret.adminPasswordMtime (not $v.configs.secret.adminPasswordHash) -}}
{{- fail "configs.secret.adminPasswordMtime needs configs.secret.adminPasswordHash" -}}
{{- end -}}
{{- if and $v.configs.secret.adminPasswordHash (not $v.configs.secret.create) -}}
{{- fail "configs.secret.adminPasswordHash needs configs.secret.create" -}}
{{- end -}}
{{- if and $v.configs.secret.adminPasswordHash (not (regexMatch "^\\$2[aby]\\$[0-9]{2}\\$[./A-Za-z0-9]{53}$" $v.configs.secret.adminPasswordHash)) -}}
{{- fail "configs.secret.adminPasswordHash must be a bcrypt hash ($2a$/$2b$/$2y$), never a password" -}}
{{- end -}}
{{- $grace := dict "server" $v.server "controller" $v.controller "repoServer" $v.repoServer "appsetController" $v.appsetController "notificationsController" $v.notificationsController "commitServer" $v.commitServer -}}
{{- range $name, $c := $grace -}}
{{- if le (int $c.terminationGracePeriodSeconds) (add (int $v.global.shutdownTimeoutSeconds) 5 | int) -}}
{{- fail (printf "%s.terminationGracePeriodSeconds (%d) must exceed global.shutdownTimeoutSeconds + 5 (%d)" $name (int $c.terminationGracePeriodSeconds) (add (int $v.global.shutdownTimeoutSeconds) 5)) -}}
{{- end -}}
{{- end -}}
{{- /* A component whose ServiceAccount the chart does not create needs its name: the chart binds
the component's Roles and ClusterRoles (cluster-admin-equivalent for the controller) to it and
must never bind them to the namespace's "default" ServiceAccount. */ -}}
{{- $sas := dict "server" $v.server.enabled "controller" $v.controller.enabled "repoServer" $v.repoServer.enabled "appsetController" $v.appsetController.enabled "notificationsController" $v.notificationsController.enabled "commitServer" (eq (include "patrol.commitServerEnabled" .) "true") "dex" $v.dex.enabled -}}
{{- range $name, $enabled := $sas -}}
{{- $sa := (index $v $name).serviceAccount -}}
{{- if and $enabled (not $sa.create) (not $sa.name) -}}
{{- fail (printf "%s.serviceAccount.name is required when %s.serviceAccount.create is false: name the existing ServiceAccount the chart binds the component's RBAC to (it never uses the namespace's default ServiceAccount, which every pod without serviceAccountName shares)" $name $name) -}}
{{- end -}}
{{- end -}}
{{- /* HorizontalPodAutoscalers: with no metric Kubernetes scales on 80% CPU, which no value
asks for; minReplicas above maxReplicas is refused by the API server. */ -}}
{{- range $name, $c := dict "server" $v.server "repoServer" $v.repoServer -}}
{{- $a := $c.autoscaling -}}
{{- if and $c.enabled $a.enabled -}}
{{- if gt (int $a.minReplicas) (int $a.maxReplicas) -}}
{{- fail (printf "%s.autoscaling.minReplicas (%d) must not exceed %s.autoscaling.maxReplicas (%d)" $name (int $a.minReplicas) $name (int $a.maxReplicas)) -}}
{{- end -}}
{{- if not (or (gt (int $a.targetCPUUtilizationPercentage) 0) (gt (int $a.targetMemoryUtilizationPercentage) 0)) -}}
{{- fail (printf "%s.autoscaling needs targetCPUUtilizationPercentage or targetMemoryUtilizationPercentage above 0 (an HPA without metrics scales on 80%% CPU)" $name) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- $names := dict -}}
{{- range $v.repoServer.cmp.plugins -}}
{{- if hasKey $names .name -}}
{{- fail (printf "repoServer.cmp.plugins: duplicate name %q" .name) -}}
{{- end -}}
{{- $_ := set $names .name true -}}
{{- if and .configMap .pluginConfig -}}
{{- fail (printf "repoServer.cmp.plugins[%s]: set configMap or pluginConfig, not both" .name) -}}
{{- end -}}
{{- if not (or .configMap .pluginConfig) -}}
{{- fail (printf "repoServer.cmp.plugins[%s]: configMap or pluginConfig is required" .name) -}}
{{- end -}}
{{- end -}}
{{- end -}}
