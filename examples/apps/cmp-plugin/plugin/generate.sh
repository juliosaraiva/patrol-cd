#!/bin/sh
# Renders every *.yaml.tmpl file of the application directory (the working directory),
# replacing @@NAME@@ placeholders. POSIX sh + awk only, so any small image works.
#
# Inputs set by Patrol CD for the generate command:
#   PATROL_APP_NAME (ARGOCD_APP_NAME)  Application name
#   ARGOCD_ENV_ENVIRONMENT             spec.source.plugin.env entry ENVIRONMENT
#   PARAM_IMAGE, PARAM_REPLICAS        string parameters
#   PARAM_EXTRA_LABELS_<KEY>           entries of the extra-labels map parameter
#                                      (keys arrive upper-cased, '-' as '_')
set -eu

fail() { echo "tmpl-render: $*" >&2; exit 1; }

# Only conservative characters are accepted, so a parameter can never change the YAML
# structure of the output.
check() {
  case "$2" in
    '' | *[!A-Za-z0-9._:/@-]*) fail "invalid value for $1: '$2'" ;;
  esac
}

app="${PATROL_APP_NAME:-${ARGOCD_APP_NAME:-}}"
image="${PARAM_IMAGE:-}"
replicas="${PARAM_REPLICAS:-1}"
environment="${ARGOCD_ENV_ENVIRONMENT:-dev}"
check "application name" "$app"
check image "$image"
check environment "$environment"
case "$replicas" in '' | *[!0-9]*) fail "replicas must be a non-negative integer: '$replicas'" ;; esac

labels=""
for kv in $(env | sed -n 's/^PARAM_EXTRA_LABELS_//p' | sort); do
  case "$kv" in *=*) ;; *) fail "malformed extra-labels entry" ;; esac
  key=$(printf '%s' "${kv%%=*}" | tr 'A-Z_' 'a-z-')
  value=${kv#*=}
  check "extra-labels key" "$key"
  check "extra-labels value for $key" "$value"
  labels="${labels}example.com/${key}: \"${value}\"\\n"
done

found=0
for f in *.yaml.tmpl; do
  [ -f "$f" ] || continue
  found=1
  echo "---"
  awk -v app="$app" -v image="$image" -v replicas="$replicas" -v environment="$environment" -v labels="$labels" '
    /@@EXTRA_LABELS@@/ {
      match($0, /^ */)
      indent = substr($0, 1, RLENGTH)
      n = split(labels, l, "\n")
      for (i = 1; i <= n; i++) if (l[i] != "") print indent l[i]
      next
    }
    {
      gsub(/@@APP_NAME@@/, app)
      gsub(/@@IMAGE@@/, image)
      gsub(/@@REPLICAS@@/, replicas)
      gsub(/@@ENVIRONMENT@@/, environment)
      print
    }' "$f"
done
[ "$found" -eq 1 ] || fail "no *.yaml.tmpl file in $(pwd)"
