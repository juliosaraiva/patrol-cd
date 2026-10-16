#!/bin/sh
# Exec KRM function: echoes the build environment kustomize was started with.
cat >/dev/null
cat <<EOF
apiVersion: config.kubernetes.io/v1
kind: ResourceList
items:
- apiVersion: v1
  kind: ConfigMap
  metadata:
    name: build-env
  data:
    ARGOCD_APP_NAME: "${ARGOCD_APP_NAME}"
    ARGOCD_APP_NAMESPACE: "${ARGOCD_APP_NAMESPACE}"
    ARGOCD_APP_REVISION_SHORT: "${ARGOCD_APP_REVISION_SHORT}"
    PATROL_APP_NAME: "${PATROL_APP_NAME}"
    PATROL_APP_SOURCE_PATH: "${PATROL_APP_SOURCE_PATH}"
    GIT_SSL_CAINFO_CONTENT: "$(cat "${GIT_SSL_CAINFO:-/dev/null}")"
    PROXY_ENV: "$(env | grep -i '^[a-z]*_proxy=' | LC_ALL=C sort | tr '\n' ' ')"
EOF
