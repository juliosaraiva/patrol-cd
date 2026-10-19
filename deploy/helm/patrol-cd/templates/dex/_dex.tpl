{{/*
Dex configuration rendered by the chart: the issuer behind patrol-server's /api/dex proxy, memory
storage, plain HTTP inside the cluster (the hop is restricted by NetworkPolicy), telemetry for
the probes, and the static clients patrol-server ("patrol", confidential: secret from
dex.clientSecretRef through secretEnv) and the CLI ("patrol-cli", public) use. dex.config is
merged over it (lists such as connectors replace the defaults).
*/}}
{{- define "patrol.dexConfig" -}}
{{- $url := trimSuffix "/" (index .Values.configs.cm "url") -}}
{{- $redirects := list (printf "%s/api/v1/auth/callback" $url) -}}
{{- with (index .Values.configs.cm "additionalUrls") -}}
{{- range (fromYamlArray .) }}{{ $redirects = append $redirects (printf "%s/api/v1/auth/callback" (trimSuffix "/" .)) }}{{ end -}}
{{- end -}}
{{- $base := dict
  "issuer" (printf "%s/api/dex" $url)
  "storage" (dict "type" "memory")
  "web" (dict "http" "0.0.0.0:5556")
  "grpc" (dict "addr" "0.0.0.0:5557")
  "telemetry" (dict "http" "0.0.0.0:5558")
  "oauth2" (dict "skipApprovalScreen" true)
  "staticClients" (list
    (dict "id" "patrol" "name" "Patrol CD" "secretEnv" "PATROL_DEX_CLIENT_SECRET" "redirectURIs" $redirects)
    (dict "id" "patrol-cli" "name" "Patrol CD CLI" "public" true "redirectURIs" (list "http://localhost:8085/auth/callback")))
-}}
{{- toYaml (mergeOverwrite $base (deepCopy (.Values.dex.config | default dict))) -}}
{{- end -}}
