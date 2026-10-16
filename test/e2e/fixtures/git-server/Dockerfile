# patrol-cd/e2e-git-server:dev — e2e git server: smart HTTP (nginx + fcgiwrap + git-http-backend)
# and SSH (OpenSSH + git-shell) over the same repositories, plus the seeding script and seed
# content used by the git-seed Job. Built by test/e2e/fixtures/build-images.sh, which supplies
# three named build contexts:
#   seed          repositories to create (one directory per repository; also combined into apps.git)
#   seed-updates  per-repository overlays for the second seeded revision
#   seed-extra    further repositories (e.g. examples/ as patrol-examples); may be empty
# Network: "apk add" downloads Alpine packages, so the first build (and any rebuild after the
# build cache is pruned) needs network access. It is the first instruction and precedes every
# COPY of local content, so later rebuilds reuse it from the build cache. No "# syntax=" line:
# the dockerfile frontend built into BuildKit is used, nothing is pulled for it.
FROM alpine:3.24.2@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6

# adduser -D leaves the account locked ("!"), which sshd refuses even for public keys, so the
# password field is set to "*" (no password, not locked).
RUN set -eux; \
    apk add --no-cache apache2-utils fcgiwrap git git-daemon nginx openssh-server su-exec tini; \
    test -x /usr/libexec/git-core/git-http-backend; \
    test -x /usr/libexec/git-core/git-shell; \
    addgroup -g 1000 git; \
    adduser -D -u 1000 -G git -h /git -s /usr/libexec/git-core/git-shell git; \
    sed -i 's/^git:!:/git:*:/' /etc/shadow; \
    echo /usr/libexec/git-core/git-shell >> /etc/shells; \
    rm -f /etc/nginx/http.d/default.conf

COPY rootfs/ /
RUN chmod 0755 /usr/local/bin/*

COPY --from=seed . /seed/repos/
COPY --from=seed-updates . /seed/updates/
COPY --from=seed-extra . /seed/extra/

EXPOSE 8080 2222
ENTRYPOINT ["/sbin/tini", "--"]
CMD ["/usr/local/bin/git-http-entrypoint"]
