# patrol-cd/e2e-toolbox:dev — client tools for e2e checks and manual debugging inside the kind
# clusters (git, ssh, helm, oras, kubectl, curl, jq, openssl). Used by the toolbox Pod, by
# test/e2e/fixtures/smoke.sh and, attached to the kind docker network, by oci/push.sh to push the
# plain-manifest OCI bundle with oras.
# Network: "apk add" needs network access on the first build (helm and kubectl are Alpine
# community packages); it precedes the oras COPY, so later rebuilds reuse it from the build cache.
FROM ghcr.io/oras-project/oras:v1.3.4 AS oras

FROM alpine:3.24.2@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6
RUN set -eux; \
    apk add --no-cache bash ca-certificates curl git helm jq kubectl openssh-client openssl tar; \
    addgroup -g 1000 toolbox; \
    adduser -D -u 1000 -G toolbox -h /home/toolbox -s /bin/sh toolbox
COPY --from=oras /bin/oras /usr/local/bin/oras
RUN oras version
USER 1000:1000
WORKDIR /home/toolbox
CMD ["sh", "-c", "trap 'exit 0' TERM INT; while :; do sleep 3600 & wait $!; done"]
