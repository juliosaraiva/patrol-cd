{{/*
NetworkPolicy fragments. Every policy selects one component's pods and lists both Ingress and
Egress, so anything not allowed here is denied for those pods.
*/}}

{{/* Peer selecting the pods of a component of this release. Usage: include "patrol.np.peer" (list $ "repo-server") */}}
{{- define "patrol.np.peer" -}}
- podSelector:
    matchLabels:
      {{- include "patrol.selectorLabels" . | nindent 6 }}
{{- end -}}

{{/* TCP ports list. Usage: include "patrol.np.ports" (list 8081 8084) */}}
{{- define "patrol.np.ports" -}}
{{- $ports := list -}}
{{- range . }}{{ $ports = append $ports (dict "port" (int .) "protocol" "TCP") }}{{ end -}}
{{- toYaml $ports -}}
{{- end -}}

{{/* DNS egress rule. */}}
{{- define "patrol.np.dns" -}}
- to:
    {{- toYaml .Values.networkPolicy.dns.to | nindent 4 }}
  ports:
    {{- toYaml .Values.networkPolicy.dns.ports | nindent 4 }}
{{- end -}}

{{/*
A CIDR as JSON {"v6": bool, "bits": prefix length, "groups": [16-bit groups of the address]}
(IPv4: 2 groups, IPv6: 8). values.schema.json checks the syntax; this fails on what a pattern
cannot (group count, "::" use). Usage: include "patrol.np.parseCIDR" "10.0.0.0/8" | fromJson
*/}}
{{- define "patrol.np.parseCIDR" -}}
{{- $parts := splitList "/" (toString .) -}}
{{- if ne (len $parts) 2 -}}{{- fail (printf "networkPolicy: %q is not a CIDR (address/prefix)" .) -}}{{- end -}}
{{- $addr := index $parts 0 -}}
{{- $bits := atoi (index $parts 1) -}}
{{- $groups := list -}}
{{- $v6 := contains ":" $addr -}}
{{- if $v6 -}}
{{- $halves := splitList "::" $addr -}}
{{- if gt (len $halves) 2 -}}{{- fail (printf "networkPolicy: %q uses \"::\" more than once" .) -}}{{- end -}}
{{- $head := list -}}
{{- if index $halves 0 }}{{ $head = splitList ":" (index $halves 0) }}{{ end -}}
{{- $tail := list -}}
{{- if and (eq (len $halves) 2) (index $halves 1) }}{{ $tail = splitList ":" (index $halves 1) }}{{ end -}}
{{- $fill := sub 8 (add (len $head) (len $tail)) | int -}}
{{- if or (and (eq (len $halves) 1) (ne $fill 0)) (and (eq (len $halves) 2) (lt $fill 1)) -}}
{{- fail (printf "networkPolicy: %q is not an IPv6 CIDR (8 groups, or fewer with \"::\")" .) -}}
{{- end -}}
{{- range $head }}{{ $groups = append $groups . }}{{ end -}}
{{- range until $fill }}{{ $groups = append $groups "0" }}{{ end -}}
{{- range $tail }}{{ $groups = append $groups . }}{{ end -}}
{{- $nums := list -}}
{{- range $groups -}}
{{- if not (regexMatch "^[0-9a-fA-F]{1,4}$" .) -}}{{- fail (printf "networkPolicy: %q has an invalid IPv6 group %q" $ .) -}}{{- end -}}
{{- $nums = append $nums (int (printf "0x%s" .)) -}}
{{- end -}}
{{- $groups = $nums -}}
{{- else -}}
{{- $o := splitList "." $addr -}}
{{- if ne (len $o) 4 -}}{{- fail (printf "networkPolicy: %q is not an IPv4 CIDR" .) -}}{{- end -}}
{{- $groups = list (add (mul (atoi (index $o 0)) 256) (atoi (index $o 1))) (add (mul (atoi (index $o 2)) 256) (atoi (index $o 3))) -}}
{{- end -}}
{{- toJson (dict "v6" $v6 "bits" $bits "groups" $groups) -}}
{{- end -}}

{{/*
"true" when CIDR inner is a strict subset of CIDR outer (same family, longer prefix, same
network bits): the API server's rule for ipBlock.except. Usage: include "patrol.np.within" (list outer inner)
*/}}
{{- define "patrol.np.within" -}}
{{- $outer := include "patrol.np.parseCIDR" (index . 0) | fromJson -}}
{{- $inner := include "patrol.np.parseCIDR" (index . 1) | fromJson -}}
{{- $pow := list 1 2 4 8 16 32 64 128 256 512 1024 2048 4096 8192 16384 32768 65536 -}}
{{- if and (eq $outer.v6 $inner.v6) (lt (int $outer.bits) (int $inner.bits)) -}}
{{- $same := true -}}
{{- range $i, $g := $outer.groups -}}
{{- $left := sub (int $outer.bits) (mul 16 $i) | int -}}
{{- if le $left 0 }}{{ break }}{{ end -}}
{{- $d := index $pow (ternary 0 (sub 16 $left | int) (ge $left 16)) -}}
{{- if ne (div (int $g) $d) (div (int (index $inner.groups $i)) $d) }}{{ $same = false }}{{ end -}}
{{- end -}}
{{- if $same }}true{{ end -}}
{{- end -}}
{{- end -}}

{{/*
Egress rule to CIDRs on TCP ports, or "" without CIDRs or ports. Each except entry is attached
only to the CIDRs that strictly contain it (the API server rejects any other except): an entry
outside every CIDR excludes nothing and is omitted, and a CIDR listed next to a wider one is
not reduced by an except that equals or contains it, which is how a range is added back
(cidrs [0.0.0.0/0, 10.20.0.0/16] with except [10.0.0.0/8] allows 10.20.0.0/16).
Usage: include "patrol.np.cidrs" (dict "cidrs" .. "except" .. "ports" ..)
*/}}
{{- define "patrol.np.cidrs" -}}
{{- if and .cidrs .ports -}}
- to:
    {{- range .cidrs }}
    {{- $cidr := . }}
    - ipBlock:
        cidr: {{ $cidr }}
        {{- $except := list }}
        {{- range ($.except | default list | uniq) }}{{ if include "patrol.np.within" (list $cidr .) }}{{ $except = append $except . }}{{ end }}{{ end }}
        {{- with $except }}
        except:
          {{- toYaml . | nindent 10 }}
        {{- end }}
    {{- end }}
  ports:
    {{- include "patrol.np.ports" .ports | nindent 4 }}
{{- end -}}
{{- end -}}

{{/* Egress to the Kubernetes API server. */}}
{{- define "patrol.np.kubeAPI" -}}
{{- include "patrol.np.cidrs" (dict "cidrs" .Values.networkPolicy.kubeAPIServer.cidrs "ports" .Values.networkPolicy.kubeAPIServer.ports) -}}
{{- end -}}

{{/*
Egress rule to the OTLP collector, or "" without one. The address is the effective otlp.address
parameter (tracing.otlpAddress or configs.params); the port is tracing.networkPolicy.port, else
the address's port, else 4317 (OTLP/gRPC). Peers: tracing.networkPolicy.to ([]: any).
*/}}
{{- define "patrol.np.tracing" -}}
{{- $addr := index (include "patrol.paramsData" . | fromYaml) "otlp.address" | default "" | toString | trim -}}
{{- if $addr -}}
{{- $np := .Values.tracing.networkPolicy -}}
{{- $port := int ($np.port | default 0) -}}
{{- if not $port -}}
{{- $m := regexFind ":[0-9]+$" $addr -}}
{{- $port = ternary (int (trimPrefix ":" $m)) 4317 (ne $m "") -}}
{{- end -}}
{{- if $np.to -}}
- to:
    {{- toYaml $np.to | nindent 4 }}
  ports:
    {{- include "patrol.np.ports" (list $port) | nindent 4 }}
{{- else -}}
- ports:
    {{- include "patrol.np.ports" (list $port) | nindent 4 }}
{{- end -}}
{{- end -}}
{{- end -}}

{{/* Ingress rule for metrics scraping, or "" when networkPolicy.metricsFrom is empty. Usage: include "patrol.np.metrics" (list $ 8083) */}}
{{- define "patrol.np.metrics" -}}
{{- $ctx := index . 0 -}}
{{- with $ctx.Values.networkPolicy.metricsFrom -}}
- from:
    {{- toYaml . | nindent 4 }}
  ports:
    {{- include "patrol.np.ports" (list (index $ 1)) | nindent 4 }}
{{- end -}}
{{- end -}}

{{/* Extra egress rules of a component, or "". Usage: include "patrol.np.extra" (list $ "server") */}}
{{- define "patrol.np.extra" -}}
{{- $ctx := index . 0 -}}
{{- with (index ($ctx.Values.networkPolicy.extraEgress | default dict) (index . 1)) -}}
{{ toYaml . }}
{{- end -}}
{{- end -}}

{{/*
NetworkPolicy object. Usage:
include "patrol.np.policy" (list $ "component" ingressRulesYAML egressRulesYAML)
*/}}
{{- define "patrol.np.policy" -}}
{{- $ctx := index . 0 -}}
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  {{- include "patrol.metadata" (list $ctx (include "patrol.name" (list $ctx (index . 1))) (index . 1) dict dict) | nindent 2 }}
spec:
  podSelector:
    matchLabels:
      {{- include "patrol.selectorLabels" (list $ctx (index . 1)) | nindent 6 }}
  policyTypes:
    - Ingress
    - Egress
  {{- with (index . 2 | trim) }}
  ingress:
    {{- . | nindent 4 }}
  {{- else }}
  ingress: []
  {{- end }}
  {{- with (index . 3 | trim) }}
  egress:
    {{- . | nindent 4 }}
  {{- else }}
  egress: []
  {{- end }}
{{- end -}}
