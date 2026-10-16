# patrol-cd/e2e-chart-repo:dev — static Helm chart repository (nginx). The charts and index.yaml
# come from the named build context "charts", produced by chart-repo/build.sh (helm package +
# helm repo index). index.yaml uses relative chart URLs, so the same files are served at
#   /          anonymous
#   /private/  basic auth (htpasswd generated at start from the chart-repo-auth Secret)
# Network: "apk add" needs network access on the first build; it precedes every COPY, so later
# rebuilds reuse it from the build cache.
FROM alpine:3.24.2@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6

RUN apk add --no-cache apache2-utils nginx tini && rm -f /etc/nginx/http.d/default.conf

COPY rootfs/ /
RUN chmod 0755 /usr/local/bin/*
COPY --from=charts . /srv/charts/

EXPOSE 8080
ENTRYPOINT ["/sbin/tini", "--"]
CMD ["/usr/local/bin/chart-repo-entrypoint"]
