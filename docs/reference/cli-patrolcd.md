# `patrolcd` command reference

`patrolcd` is the Patrol CD control-plane multi-call binary (one image, one binary, one
subcommand per component). The `patrol` CLI is a separate binary. Architecture, dependency
graphs, ports and environment variables: [wiring.md](../architecture/components/wiring.md).

The help texts below are the output of the binary built from this tree; they are pinned by
`cmd/patrolcd/testdata/help/*.golden` (`go test ./cmd/patrolcd -run TestHelpGolden`, `-update`
regenerates them), so this page and the binary cannot drift silently.

## Conventions

- **Precedence** of every `patrol-cmd-params-cm` parameter: command-line flag > environment
  variable (`PATROL_*`, named in each flag's help) > `patrol-cmd-params-cm` > compiled default.
  Flags that are not parameters (ports, directories, routing) have no environment variable
  unless their help names one.
- **Defaults** shown are the compiled defaults. `--logformat` shows `json`; when the format
  is configured nowhere, a process outside a pod logs text.
- **Booleans** accept `--flag` or `--flag=true|false`; lists are comma-separated; maps are
  `k1=v1,k2=v2`; quantities use Kubernetes syntax (`10M`, `1G`); durations Go syntax
  (`90s`) or whole days (`2d`).
- **Binary names**: when started as `patrol-server`, `patrol-controller`,
  `patrol-application-controller`, `patrol-repo-server`, `patrol-applicationset-controller`,
  `patrol-appset-controller`, `patrol-notifications`, `patrol-notifications-controller`,
  `patrol-cmp-server`, `patrol-commit-server` or `patrol-k8s-auth` (symbolic links), or with
  `PATROL_BINARY_NAME` set to one of these names, the matching subcommand runs.
- **Hidden modes** (never invoked by hand): `__patrol-sandbox-exec` (rlimit helper),
  `jsonnet-eval` (Jsonnet evaluator child), the git askpass prompt answerer
  (`PATROL_GIT_ASKPASS_FILE` set).
- **Signals**: SIGTERM/SIGINT start a graceful drain bounded by `--shutdown-timeout`; a second
  signal terminates immediately. Exit status 0 after a clean shutdown, 1 on errors (including
  invalid flags and invalid fatal parameters).
- **Hidden flags**: deprecated parameters (e.g. `--repo-server-strict-tls`, use
  `--repo-server-ca-cert-path`) and parameters without effect in Patrol CD (Redis, gRPC
  service config) are accepted but not listed.

## patrolcd

```text
patrolcd runs the Patrol CD control-plane components. Each subcommand is one component;
the image also links the binary under each component name (patrol-server, patrol-repo-server, ...).

Configuration precedence for every patrol-cmd-params-cm parameter:
  command-line flag > environment variable (PATROL_*) > patrol-cmd-params-cm > default

Usage:
  patrolcd [flags]
  patrolcd [command]

Available Commands:
  appset-controller        Run the ApplicationSet controller
  bootstrap                Create the first-run Secrets and the default SSH known hosts when absent
  cmp-server               Run a config management plugin sidecar server
  commit-server            Run the commit-server (source hydrator writes; no Kubernetes access)
  controller               Run the application controller (reconcilers, sharding, tree API, source hydrator)
  help                     Help about any command
  k8s-auth                 Print Kubernetes exec credentials for managed cloud clusters
  notifications-controller Run the notifications controller
  repo-server              Run the repo-server (sources and manifest rendering; no Kubernetes access)
  server                   Run patrol-server (API, web UI, webhooks)
  version                  Print version information

Flags:
  -h, --help   help for patrolcd

Use "patrolcd [command] --help" for more information about a command.
```

## patrolcd server

patrol-server: public API (`/api/v1`), web UI, SSO, Dex proxy (`/api/dex`), webhooks (`/api/webhook`), UI extensions (`/extensions.js`, `/extensions/…`), CLI downloads (`/download/…`).

```text
Run patrol-server (API, web UI, webhooks)

Usage:
  patrolcd server [flags]

Flags:
      --address string                                Listen address; env PATROL_SERVER_LISTEN_ADDRESS, patrol-cmd-params-cm server.listen.address (default "0.0.0.0")
      --allow-cluster-exec                            Let cluster connection checks run exec credential plugins and use the server's own cloud identity
      --allowed-scm-providers strings                 Allowed SCM provider API URLs; env PATROL_APPLICATIONSET_CONTROLLER_ALLOWED_SCM_PROVIDERS, patrol-cmd-params-cm applicationsetcontroller.allowed.scm.providers
      --api-content-types string                      Semicolon-separated content types allowed on non-GET requests (empty allows all); env PATROL_API_CONTENT_TYPES, patrol-cmd-params-cm server.api.content.types (default "application/json")
      --app-state-cache-expiration duration           Application state cache TTL; env PATROL_APP_STATE_CACHE_EXPIRATION, patrol-cmd-params-cm server.app.state.cache.expiration (default 1h)
      --application-namespaces strings                Additional namespaces (globs) Applications may live in; env PATROL_APPLICATION_NAMESPACES, patrol-cmd-params-cm application.namespaces
      --basehref string                               UI base href; env PATROL_SERVER_BASEHREF, patrol-cmd-params-cm server.basehref (default "/")
      --connection-status-cache-expiration duration   Repository/cluster connection status cache TTL; env PATROL_SERVER_CONNECTION_STATUS_CACHE_EXPIRATION, patrol-cmd-params-cm server.connection.status.cache.expiration (default 1h)
      --content-security-policy string                Content-Security-Policy header (empty disables); env PATROL_SERVER_CONTENT_SECURITY_POLICY, patrol-cmd-params-cm server.content.security.policy (default "frame-ancestors 'self';")
      --context string                                Kubeconfig context to use
      --controller-service string                     Headless Service of the controller StatefulSet (tree API routing) (default "patrol-controller")
      --controller-statefulset string                 Controller StatefulSet name (tree API routing) (default "patrol-controller")
      --controller-tree-api-ca-cert-path string       CA bundle verifying the controller tree API certificate (default: not verified; the internal token authenticates)
      --controller-tree-api-port int                  Port of the controller tree API (default 8090)
      --controller-tree-api-tls                       Use TLS to reach the controller tree API
      --controller-tree-api-url string                Send every controller tree API call to this URL (single controller, development); default: route to the replica owning the shard
      --default-cache-expiration duration             Default cache TTL; env PATROL_DEFAULT_CACHE_EXPIRATION, patrol-cmd-params-cm server.default.cache.expiration (default 24h)
      --dex-server string                             Dex address; env PATROL_SERVER_DEX_SERVER, patrol-cmd-params-cm server.dex.server (default "patrol-dex-server:5556")
      --dex-server-ca-cert-path string                CA bundle verifying Dex's certificate with --dex-server-strict-tls (default "/app/config/dex/tls/ca.crt")
      --dex-server-cert-path string                   Dex's certificate, pinned with --dex-server-strict-tls (Dex must present exactly this certificate) (default "/app/config/dex/tls/tls.crt")
      --dex-server-plaintext                          Connect to Dex without TLS; env PATROL_SERVER_DEX_SERVER_PLAINTEXT, patrol-cmd-params-cm server.dex.server.plaintext
      --dex-server-strict-tls                         Verify Dex's TLS certificate; env PATROL_SERVER_DEX_SERVER_STRICT_TLS, patrol-cmd-params-cm server.dex.server.strict.tls
      --disable-auth                                  Disable authentication and authorization; env PATROL_SERVER_DISABLE_AUTH, patrol-cmd-params-cm server.disable.auth
      --download-dir string                           Directory of CLI binaries served under /download/ (patrol-<os>-<arch>) (default "/shared/download")
      --enable-github-api-metrics                     GitHub API metrics; env PATROL_APPLICATIONSET_CONTROLLER_ENABLE_GITHUB_API_METRICS, patrol-cmd-params-cm applicationsetcontroller.enable.github.api.metrics
      --enable-gzip                                   Gzip HTTP responses; env PATROL_SERVER_ENABLE_GZIP, patrol-cmd-params-cm server.enable.gzip (default true)
      --enable-new-git-file-globbing                  New git files generator globbing; env PATROL_APPLICATIONSET_CONTROLLER_ENABLE_NEW_GIT_FILE_GLOBBING, patrol-cmd-params-cm applicationsetcontroller.enable.new.git.file.globbing
      --enable-pprof                                  Serve /debug/pprof/ on the metrics listener (otherwise only while the file named by $PATROL_ENABLE_PROFILER_FILE_PATH contains "true")
      --enable-proxy-extension                        Enable proxy extensions; env PATROL_SERVER_ENABLE_PROXY_EXTENSION, patrol-cmd-params-cm server.enable.proxy.extension
      --enable-scm-providers                          Enable SCM provider and pull request generators; env PATROL_APPLICATIONSET_CONTROLLER_ENABLE_SCM_PROVIDERS, patrol-cmd-params-cm applicationsetcontroller.enable.scm.providers (default true)
      --extensions-dir string                         Directory of UI extensions (extension*.js concatenated into /extensions.js; other files served under /extensions/) (default "/tmp/extensions")
      --glob-cache-size int                           RBAC glob cache size (no effect: patterns are compiled with the policy); env PATROL_SERVER_GLOB_CACHE_SIZE, patrol-cmd-params-cm server.glob.cache.size (default 10000)
      --healthz-addr string                           Separate listener serving /healthz, /readyz and /livez ("" or "-": the metrics listener only)
  -h, --help                                          help for server
      --hydrator-enabled                              Enable the source hydrator; env PATROL_HYDRATOR_ENABLED, patrol-cmd-params-cm hydrator.enabled
      --insecure                                      Serve without TLS; env PATROL_SERVER_INSECURE, patrol-cmd-params-cm server.insecure
      --internal-token-file string                    File holding the shared internal token (default $PATROL_INTERNAL_TOKEN_FILE or /app/config/internal-token/token)
      --kubeconfig string                             Path to a kubeconfig file (default: in-cluster configuration, else $KUBECONFIG or ~/.kube/config)
      --logformat string                              Log format (json or text, any case) (one of json, text); env PATROL_SERVER_LOGFORMAT, patrol-cmd-params-cm server.log.format (default "json")
      --loglevel level                                Log level (a logrus level name, any case); env PATROL_SERVER_LOG_LEVEL, patrol-cmd-params-cm server.log.level (default info)
      --metrics-addr string                           Address of the metrics, probe and profiler listener ("-" disables it) (default: --metrics-address and --metrics-port)
      --metrics-address string                        Metrics listen address; env PATROL_SERVER_METRICS_LISTEN_ADDRESS, patrol-cmd-params-cm server.metrics.listen.address (default "0.0.0.0")
      --metrics-port int                              Port of the metrics listener (default 8083)
  -n, --namespace string                              Control-plane namespace (default: the pod namespace, else the kubeconfig context namespace)
      --oidc-cache-expiration duration                OIDC state cache TTL; env PATROL_SERVER_OIDC_CACHE_EXPIRATION, patrol-cmd-params-cm server.oidc.cache.expiration (default 3m)
      --otlp-address string                           OpenTelemetry collector address; env PATROL_SERVER_OTLP_ADDRESS, patrol-cmd-params-cm otlp.address
      --otlp-attrs strings                            OpenTelemetry attributes (key:value,...); env PATROL_SERVER_OTLP_ATTRS, patrol-cmd-params-cm otlp.attrs
      --otlp-headers key=value                        OpenTelemetry headers (k1=v1,k2=v2); env PATROL_SERVER_OTLP_HEADERS, patrol-cmd-params-cm otlp.headers
      --otlp-insecure                                 OpenTelemetry collector without TLS; env PATROL_SERVER_OTLP_INSECURE, patrol-cmd-params-cm otlp.insecure (default true)
      --port int                                      Port of the API, UI and webhook listener (default 8080)
      --repo-server string                            Repo-server address; env PATROL_SERVER_REPO_SERVER, patrol-cmd-params-cm repo.server (default "patrol-repo-server:8081")
      --repo-server-ca-cert-path string               CA bundle verifying the repo-server; env PATROL_SERVER_REPO_SERVER_CA_CERT_PATH, patrol-cmd-params-cm server.repo.server.ca.cert.path
      --repo-server-client-cert-key-path string       mTLS client key; env PATROL_SERVER_REPO_SERVER_CLIENT_CERT_KEY_PATH, patrol-cmd-params-cm server.repo.server.client.cert.key.path (default "/app/config/reposerver/mtls/client.key")
      --repo-server-client-cert-path string           mTLS client certificate; env PATROL_SERVER_REPO_SERVER_CLIENT_CERT_PATH, patrol-cmd-params-cm server.repo.server.client.cert.path (default "/app/config/reposerver/mtls/client.crt")
      --repo-server-plaintext                         Connect to the repo-server without TLS; env PATROL_SERVER_REPO_SERVER_PLAINTEXT, patrol-cmd-params-cm server.repo.server.plaintext
      --repo-server-timeout-seconds int               Repo-server request timeout in seconds; env PATROL_SERVER_REPO_SERVER_TIMEOUT_SECONDS, patrol-cmd-params-cm server.repo.server.timeout.seconds (default 60)
      --rootpath string                               Path prefix behind a reverse proxy; env PATROL_SERVER_ROOTPATH, patrol-cmd-params-cm server.rootpath
      --scm-root-ca-path string                       CA bundle for SCM providers; env PATROL_APPLICATIONSET_CONTROLLER_SCM_ROOT_CA_PATH, patrol-cmd-params-cm applicationsetcontroller.scm.root.ca.path
      --shutdown-timeout duration                     Bound of the graceful drain after SIGTERM (default 30s)
      --staticassets string                           Additional static assets directory; env PATROL_SERVER_STATIC_ASSETS, patrol-cmd-params-cm server.staticassets (default "/shared/app")
      --sync-with-replace-allowed                     Allow Replace syncs through the API; env PATROL_SYNC_WITH_REPLACE_ALLOWED, patrol-cmd-params-cm server.sync.replace.allowed (default true)
      --tls-cert string                               TLS certificate file of the listener (reloaded when it changes)
      --tls-key string                                TLS private key file of the listener
      --tlsciphers string                             Colon-separated TLS cipher suites; env PATROL_TLS_CIPHERS, patrol-cmd-params-cm server.tls.ciphers (default "TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384")
      --tlsmaxversion string                          Maximum TLS version (one of 1.0, 1.1, 1.2, 1.3); env PATROL_TLS_MAX_VERSION, patrol-cmd-params-cm server.tls.maxversion (default "1.3")
      --tlsminversion string                          Minimum TLS version (one of 1.0, 1.1, 1.2, 1.3); env PATROL_TLS_MIN_VERSION, patrol-cmd-params-cm server.tls.minversion (default "1.2")
      --trusted-proxies strings                       CIDRs of reverse proxies whose X-Forwarded-For is trusted (rate limiting, logs, webhook source)
      --webhook-parallelism-limit int                 Concurrent webhook requests; env PATROL_SERVER_WEBHOOK_PARALLELISM_LIMIT, patrol-cmd-params-cm server.webhook.parallelism.limit (default 50)
      --webhook-refresh-workers int                   Webhook refresh workers; env PATROL_SERVER_WEBHOOK_REFRESH_WORKERS, patrol-cmd-params-cm server.webhook.refresh.workers (default 20)
      --x-frame-options string                        X-Frame-Options header (empty disables); env PATROL_SERVER_X_FRAME_OPTIONS, patrol-cmd-params-cm server.x.frame.options (default "sameorigin")
```

## patrolcd controller

Application controller: Application and Operation reconcilers, live-state caches, sync engine, Lease-based shards, tree API for patrol-server, source hydrator (`--hydrator-enabled`).

```text
Run the application controller (reconcilers, sharding, tree API, source hydrator)

Usage:
  patrolcd controller [flags]

Flags:
      --app-state-cache-expiration duration                       Application state cache TTL; env PATROL_APP_STATE_CACHE_EXPIRATION, patrol-cmd-params-cm controller.app.state.cache.expiration (default 1h)
      --application-namespaces strings                            Additional namespaces (globs) Applications may live in; env PATROL_APPLICATION_NAMESPACES, patrol-cmd-params-cm application.namespaces
      --commit-server string                                      Commit-server address; env PATROL_APPLICATION_CONTROLLER_COMMIT_SERVER, patrol-cmd-params-cm commit.server (default "patrol-commit-server:8086")
      --commit-server-ca-cert-path string                         CA bundle verifying the commit-server certificate (https commit.server; default: not verified, the internal token authenticates)
      --commit-server-client-cert-key-path string                 Key of --commit-server-client-cert-path
      --commit-server-client-cert-path string                     Client certificate presented to a commit-server requiring mutual TLS (https commit.server)
      --commit-server-timeout duration                            Bound of one source hydrator commit request (clone, write, push) (default 10m0s)
      --context string                                            Kubeconfig context to use
      --default-cache-expiration duration                         Default cache TTL; env PATROL_DEFAULT_CACHE_EXPIRATION, patrol-cmd-params-cm controller.default.cache.expiration (default 24h)
      --dynamic-cluster-distribution-enabled                      Read the shard count from the controller workload (env PATROL_ENABLE_DYNAMIC_CLUSTER_DISTRIBUTION)
      --enable-pprof                                              Serve /debug/pprof/ on the metrics listener (otherwise only while the file named by $PATROL_ENABLE_PROFILER_FILE_PATH contains "true")
      --healthz-addr string                                       Separate listener serving /healthz, /readyz and /livez ("" or "-": the metrics listener only)
  -h, --help                                                      help for controller
      --hydration-processors int                                  Hydration workers; env PATROL_APPLICATION_CONTROLLER_HYDRATION_PROCESSORS, patrol-cmd-params-cm controller.hydration.processors (default 5)
      --hydrator-enabled                                          Enable the source hydrator; env PATROL_HYDRATOR_ENABLED, patrol-cmd-params-cm hydrator.enabled
      --ignore-normalizer-jq-execution-timeout-seconds duration   Jq ignore-rule timeout (0 = normalizer default); env PATROL_IGNORE_NORMALIZER_JQ_TIMEOUT, patrol-cmd-params-cm controller.ignore.normalizer.jq.timeout (default 0s)
      --internal-token-file string                                File holding the shared internal token (default $PATROL_INTERNAL_TOKEN_FILE or /app/config/internal-token/token)
      --kubeconfig string                                         Path to a kubeconfig file (default: in-cluster configuration, else $KUBECONFIG or ~/.kube/config)
      --kubectl-parallelism-limit int                             Concurrent apply operations (<1 = unlimited); env PATROL_APPLICATION_CONTROLLER_KUBECTL_PARALLELISM_LIMIT, patrol-cmd-params-cm controller.kubectl.parallelism.limit (default 20)
      --logformat string                                          Log format (json or text, any case) (one of json, text); env PATROL_APPLICATION_CONTROLLER_LOGFORMAT, patrol-cmd-params-cm controller.log.format (default "json")
      --loglevel level                                            Log level (a logrus level name, any case); env PATROL_APPLICATION_CONTROLLER_LOGLEVEL, patrol-cmd-params-cm controller.log.level (default info)
      --metrics-addr string                                       Address of the metrics, probe and profiler listener ("-" disables it) (default: --metrics-address and --metrics-port)
      --metrics-cache-expiration duration                         Prometheus metrics cache expiration (0 disables); env PATROL_APPLICATION_CONTROLLER_METRICS_CACHE_EXPIRATION, patrol-cmd-params-cm controller.metrics.cache.expiration (default 0s)
      --metrics-port int                                          Port of the metrics listener (default 8082)
  -n, --namespace string                                          Control-plane namespace (default: the pod namespace, else the kubeconfig context namespace)
      --operation-processors int                                  Operation workers; env PATROL_APPLICATION_CONTROLLER_OPERATION_PROCESSORS, patrol-cmd-params-cm controller.operation.processors (default 10)
      --otlp-address string                                       OpenTelemetry collector address; env PATROL_APPLICATION_CONTROLLER_OTLP_ADDRESS, patrol-cmd-params-cm otlp.address
      --otlp-attrs strings                                        OpenTelemetry attributes (key:value,...); env PATROL_APPLICATION_CONTROLLER_OTLP_ATTRS, patrol-cmd-params-cm otlp.attrs
      --otlp-headers key=value                                    OpenTelemetry headers (k1=v1,k2=v2); env PATROL_APPLICATION_CONTROLLER_OTLP_HEADERS, patrol-cmd-params-cm otlp.headers
      --otlp-insecure                                             OpenTelemetry collector without TLS; env PATROL_APPLICATION_CONTROLLER_OTLP_INSECURE, patrol-cmd-params-cm otlp.insecure (default true)
      --persist-resource-health                                   Store resource health in Application status; env PATROL_APPLICATION_CONTROLLER_PERSIST_RESOURCE_HEALTH, patrol-cmd-params-cm controller.resource.health.persist
      --repo-error-grace-period-seconds duration                  Grace period for repo-server errors (a duration such as 180s); env PATROL_REPO_ERROR_GRACE_PERIOD_SECONDS, patrol-cmd-params-cm controller.repo.error.grace.period.seconds (default 180s)
      --repo-server string                                        Repo-server address; env PATROL_APPLICATION_CONTROLLER_REPO_SERVER, patrol-cmd-params-cm repo.server (default "patrol-repo-server:8081")
      --repo-server-ca-cert-path string                           CA bundle verifying the repo-server; env PATROL_APPLICATION_CONTROLLER_REPO_SERVER_CA_CERT_PATH, patrol-cmd-params-cm controller.repo.server.ca.cert.path
      --repo-server-client-cert-key-path string                   mTLS client key; env PATROL_APPLICATION_CONTROLLER_REPO_SERVER_CLIENT_CERT_KEY_PATH, patrol-cmd-params-cm controller.repo.server.client.cert.key.path (default "/app/config/reposerver/mtls/client.key")
      --repo-server-client-cert-path string                       mTLS client certificate; env PATROL_APPLICATION_CONTROLLER_REPO_SERVER_CLIENT_CERT_PATH, patrol-cmd-params-cm controller.repo.server.client.cert.path (default "/app/config/reposerver/mtls/client.crt")
      --repo-server-plaintext                                     Connect to the repo-server without TLS; env PATROL_APPLICATION_CONTROLLER_REPO_SERVER_PLAINTEXT, patrol-cmd-params-cm controller.repo.server.plaintext
      --repo-server-timeout-seconds int                           Repo-server request timeout in seconds; env PATROL_APPLICATION_CONTROLLER_REPO_SERVER_TIMEOUT_SECONDS, patrol-cmd-params-cm controller.repo.server.timeout.seconds (default 60)
      --self-heal-backoff-cap-seconds int                         Maximum self-heal backoff; env PATROL_APPLICATION_CONTROLLER_SELF_HEAL_BACKOFF_CAP_SECONDS, patrol-cmd-params-cm controller.self.heal.backoff.cap.seconds (default 300)
      --self-heal-backoff-factor int                              Self-heal backoff factor; env PATROL_APPLICATION_CONTROLLER_SELF_HEAL_BACKOFF_FACTOR, patrol-cmd-params-cm controller.self.heal.backoff.factor (default 3)
      --self-heal-backoff-timeout-seconds int                     Initial self-heal backoff (0 disables backoff); env PATROL_APPLICATION_CONTROLLER_SELF_HEAL_BACKOFF_TIMEOUT_SECONDS, patrol-cmd-params-cm controller.self.heal.backoff.timeout.seconds (default 2)
      --self-heal-timeout-seconds int                             Fixed delay between self-heal attempts; env PATROL_APPLICATION_CONTROLLER_SELF_HEAL_TIMEOUT_SECONDS, patrol-cmd-params-cm controller.self.heal.timeout.seconds
      --server-side-diff-enabled                                  Use server-side diff by default; env PATROL_APPLICATION_CONTROLLER_SERVER_SIDE_DIFF, patrol-cmd-params-cm controller.diff.server.side
      --sharding-method string                                    Cluster to shard assignment (one of legacy, round-robin, consistent-hashing); env PATROL_CONTROLLER_SHARDING_ALGORITHM, patrol-cmd-params-cm controller.sharding.algorithm (default "legacy")
      --shutdown-timeout duration                                 Bound of the graceful drain after SIGTERM (default 30s)
      --status-processors int                                     Application refresh workers; env PATROL_APPLICATION_CONTROLLER_STATUS_PROCESSORS, patrol-cmd-params-cm controller.status.processors (default 20)
      --sync-timeout int                                          Sync operation timeout (0 = none); env PATROL_APPLICATION_CONTROLLER_SYNC_TIMEOUT, patrol-cmd-params-cm controller.sync.timeout.seconds
      --tls-cert string                                           TLS certificate file of the listener (reloaded when it changes)
      --tls-client-ca string                                      CA bundle requiring and verifying client certificates (mutual TLS)
      --tls-key string                                            TLS private key file of the listener
      --tree-api-addr string                                      Address of the tree API served to patrol-server ("-" disables it); TLS with --tls-cert/--tls-key (default ":8090")
```

## patrolcd repo-server

Repo-server: git/Helm/OCI sources and manifest rendering; no Kubernetes access. Reads `patrol-cmd-params-cm` only through the environment.

```text
Run the repo-server (sources and manifest rendering; no Kubernetes access)

Usage:
  patrolcd repo-server [flags]

Flags:
      --address string                                   Listen address; env PATROL_REPO_SERVER_LISTEN_ADDRESS, patrol-cmd-params-cm reposerver.listen.address (default "0.0.0.0")
      --allow-oob-symlinks                               Allow symlinks leaving the repository; env PATROL_REPO_SERVER_ALLOW_OUT_OF_BOUNDS_SYMLINKS, patrol-cmd-params-cm reposerver.allow.oob.symlinks
      --client-ca-path string                            Client CA bundle enabling mTLS; env PATROL_REPO_SERVER_CLIENT_CA_PATH, patrol-cmd-params-cm reposerver.client.ca.path (default "/app/config/reposerver/mtls/client-ca.crt")
      --credentials-dir string                           Directory of per-command credential files (tmpfs recommended; default $TMPDIR)
      --default-cache-expiration duration                Default cache TTL; env PATROL_DEFAULT_CACHE_EXPIRATION, patrol-cmd-params-cm reposerver.default.cache.expiration (default 24h)
      --disable-helm-manifest-max-extracted-size         Disable the Helm extraction limit; env PATROL_REPO_SERVER_DISABLE_HELM_MANIFEST_MAX_EXTRACTED_SIZE, patrol-cmd-params-cm reposerver.disable.helm.manifest.max.extracted.size
      --disable-oci-manifest-max-extracted-size          Disable the OCI extraction limit; env PATROL_REPO_SERVER_DISABLE_OCI_MANIFEST_MAX_EXTRACTED_SIZE, patrol-cmd-params-cm reposerver.disable.oci.manifest.max.extracted.size
      --disable-tls                                      Serve without TLS; env PATROL_REPO_SERVER_DISABLE_TLS, patrol-cmd-params-cm reposerver.disable.tls
      --enable-builtin-git-config                        Apply built-in git configuration; env PATROL_REPO_SERVER_ENABLE_BUILTIN_GIT_CONFIG, patrol-cmd-params-cm reposerver.enable.builtin.git.config (default true)
      --enable-pprof                                     Serve /debug/pprof/ on the metrics listener (otherwise only while the file named by $PATROL_ENABLE_PROFILER_FILE_PATH contains "true")
      --gnupg-home-parent string                         Parent directory of the private GnuPG keyring (default $TMPDIR)
      --gpg-keys-dir string                              GnuPG public keys directory mounted from patrol-gpg-keys-cm ("-" disables) (default "/app/config/gpg/source")
      --helm-manifest-max-extracted-size quantity        Maximum extracted Helm chart size; env PATROL_REPO_SERVER_HELM_MANIFEST_MAX_EXTRACTED_SIZE, patrol-cmd-params-cm reposerver.helm.manifest.max.extracted.size (default 1G)
      --helm-plugins-dir string                          Directory of operator-installed Helm plugins
  -h, --help                                             help for repo-server
      --include-hidden-directories                       Include hidden directories in directory listings; env PATROL_REPO_SERVER_INCLUDE_HIDDEN_DIRECTORIES, patrol-cmd-params-cm reposerver.include.hidden.directories
      --internal-token-file string                       File holding the shared internal token (default $PATROL_INTERNAL_TOKEN_FILE or /app/config/internal-token/token)
      --logformat string                                 Log format (json or text, any case) (one of json, text); env PATROL_REPO_SERVER_LOGFORMAT, patrol-cmd-params-cm reposerver.log.format (default "json")
      --loglevel level                                   Log level (a logrus level name, any case); env PATROL_REPO_SERVER_LOGLEVEL, patrol-cmd-params-cm reposerver.log.level (default info)
      --max-combined-directory-manifests-size quantity   Maximum combined manifest size of a directory source; env PATROL_REPO_SERVER_MAX_COMBINED_DIRECTORY_MANIFESTS_SIZE, patrol-cmd-params-cm reposerver.max.combined.directory.manifests.size (default 10M)
      --max-working-copies int                           Maximum persistent working copies (least recently used evicted) (default 512)
      --metrics-addr string                              Address of the metrics, probe and profiler listener ("-" disables it) (default: --metrics-address and --metrics-port)
      --metrics-address string                           Metrics listen address; env PATROL_REPO_SERVER_METRICS_LISTEN_ADDRESS, patrol-cmd-params-cm reposerver.metrics.listen.address (default "0.0.0.0")
      --metrics-port int                                 Port of the probe and metrics listener (default 8084)
      --oci-layer-media-types strings                    Allowed OCI layer media types; env PATROL_REPO_SERVER_OCI_LAYER_MEDIA_TYPES, patrol-cmd-params-cm reposerver.oci.layer.media.types (default application/vnd.oci.image.layer.v1.tar,application/vnd.oci.image.layer.v1.tar+gzip,application/vnd.cncf.helm.chart.content.v1.tar+gzip)
      --oci-manifest-max-extracted-size quantity         Maximum extracted OCI artifact size; env PATROL_REPO_SERVER_OCI_MANIFEST_MAX_EXTRACTED_SIZE, patrol-cmd-params-cm reposerver.oci.manifest.max.extracted.size (default 1G)
      --otlp-address string                              OpenTelemetry collector address; env PATROL_REPO_SERVER_OTLP_ADDRESS, patrol-cmd-params-cm otlp.address
      --otlp-attrs strings                               OpenTelemetry attributes (key:value,...); env PATROL_REPO_SERVER_OTLP_ATTRS, patrol-cmd-params-cm otlp.attrs
      --otlp-headers key=value                           OpenTelemetry headers (k1=v1,k2=v2); env PATROL_REPO_SERVER_OTLP_HEADERS, patrol-cmd-params-cm otlp.headers
      --otlp-insecure                                    OpenTelemetry collector without TLS; env PATROL_REPO_SERVER_OTLP_INSECURE, patrol-cmd-params-cm otlp.insecure (default true)
      --parallelismlimit int                             Concurrent manifest generations (0 = unlimited); env PATROL_REPO_SERVER_PARALLELISM_LIMIT, patrol-cmd-params-cm reposerver.parallelism.limit
      --plugin-socket-dir string                         Directory of config management plugin sockets (default "/home/patrol/cmp-server/plugins")
      --plugin-tar-exclude strings                       Semicolon-separated globs excluded from plugin tarballs; env PATROL_REPO_SERVER_PLUGIN_TAR_EXCLUSIONS, patrol-cmd-params-cm reposerver.plugin.tar.exclusions
      --plugin-use-manifest-generate-paths               Send only manifest-generate-paths to plugins; env PATROL_REPO_SERVER_PLUGIN_USE_MANIFEST_GENERATE_PATHS, patrol-cmd-params-cm reposerver.plugin.use.manifest.generate.paths
      --port int                                         Port of the API listener (default 8081)
      --repo-cache-expiration duration                   Render cache TTL; env PATROL_REPO_CACHE_EXPIRATION, patrol-cmd-params-cm reposerver.repo.cache.expiration (default 24h)
      --revision-cache-expiration duration               Cache expiration of resolved revisions (default 3m0s)
      --revision-cache-lock-timeout duration             Revision cache lock timeout; env PATROL_REVISION_CACHE_LOCK_TIMEOUT, patrol-cmd-params-cm reposerver.revision.cache.lock.timeout (default 10s)
      --root-dir string                                  Working directory of clones, extracted artifacts and uploads (default $TMPDIR/_patrol-repo)
      --shutdown-timeout duration                        Bound of the graceful drain after SIGTERM (default 30s)
      --ssh-known-hosts-path string                      SSH known hosts file mounted from patrol-ssh-known-hosts-cm ("-" disables) (default "/app/config/ssh/ssh_known_hosts")
      --streamed-manifest-max-extracted-size quantity    Maximum extracted streamed manifests; env PATROL_REPO_SERVER_STREAMED_MANIFEST_MAX_EXTRACTED_SIZE, patrol-cmd-params-cm reposerver.streamed.manifest.max.extracted.size (default 1G)
      --streamed-manifest-max-tar-size quantity          Maximum streamed manifest tarball; env PATROL_REPO_SERVER_STREAMED_MANIFEST_MAX_TAR_SIZE, patrol-cmd-params-cm reposerver.streamed.manifest.max.tar.size (default 100M)
      --tls-cert string                                  TLS certificate file of the listener (reloaded when it changes)
      --tls-certs-dir string                             Repository TLS certificates directory mounted from patrol-tls-certs-cm ("-" disables) (default "/app/config/tls")
      --tls-client-ca string                             CA bundle requiring and verifying client certificates (mutual TLS)
      --tls-key string                                   TLS private key file of the listener
      --tlsciphers string                                Colon-separated TLS cipher suites; env PATROL_TLS_CIPHERS, patrol-cmd-params-cm reposerver.tls.ciphers (default "TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384")
      --tlsmaxversion string                             Maximum TLS version (one of 1.0, 1.1, 1.2, 1.3); env PATROL_TLS_MAX_VERSION, patrol-cmd-params-cm reposerver.tls.maxversion (default "1.3")
      --tlsminversion string                             Minimum TLS version (one of 1.0, 1.1, 1.2, 1.3); env PATROL_TLS_MIN_VERSION, patrol-cmd-params-cm reposerver.tls.minversion (default "1.2")
```

## patrolcd appset-controller

ApplicationSet controller (generators, templating, progressive syncs). Leader election with `--enable-leader-election` (`applicationsetcontroller.enable.leader.election`).

```text
Run the ApplicationSet controller

Usage:
  patrolcd appset-controller [flags]

Flags:
      --allowed-scm-providers strings             Allowed SCM provider API URLs; env PATROL_APPLICATIONSET_CONTROLLER_ALLOWED_SCM_PROVIDERS, patrol-cmd-params-cm applicationsetcontroller.allowed.scm.providers
      --applicationset-namespaces strings         Namespaces (globs) watched for ApplicationSets; env PATROL_APPLICATIONSET_CONTROLLER_NAMESPACES, patrol-cmd-params-cm applicationsetcontroller.namespaces
      --cache-sync-period duration                Informer resync period (default 10h0m0s)
      --concurrent-application-updates int        Parallel Application writes within one reconcile (default 1)
      --concurrent-reconciliations int            Concurrent reconciliations; env PATROL_APPLICATIONSET_CONTROLLER_CONCURRENT_RECONCILIATIONS, patrol-cmd-params-cm applicationsetcontroller.concurrent.reconciliations.max (default 10)
      --context string                            Kubeconfig context to use
      --debug                                     Debug logging (overrides log level); env PATROL_APPLICATIONSET_CONTROLLER_DEBUG, patrol-cmd-params-cm applicationsetcontroller.debug
      --dry-run                                   Do not change Applications; env PATROL_APPLICATIONSET_CONTROLLER_DRY_RUN, patrol-cmd-params-cm applicationsetcontroller.dryrun
      --enable-github-api-metrics                 GitHub API metrics; env PATROL_APPLICATIONSET_CONTROLLER_ENABLE_GITHUB_API_METRICS, patrol-cmd-params-cm applicationsetcontroller.enable.github.api.metrics
      --enable-leader-election                    Leader election; env PATROL_APPLICATIONSET_CONTROLLER_ENABLE_LEADER_ELECTION, patrol-cmd-params-cm applicationsetcontroller.enable.leader.election
      --enable-new-git-file-globbing              New git files generator globbing; env PATROL_APPLICATIONSET_CONTROLLER_ENABLE_NEW_GIT_FILE_GLOBBING, patrol-cmd-params-cm applicationsetcontroller.enable.new.git.file.globbing
      --enable-policy-override                    Allow ApplicationSets to override the policy (unset: true when policy is empty); env PATROL_APPLICATIONSET_CONTROLLER_ENABLE_POLICY_OVERRIDE, patrol-cmd-params-cm applicationsetcontroller.enable.policy.override
      --enable-pprof                              Serve /debug/pprof/ on the metrics listener (otherwise only while the file named by $PATROL_ENABLE_PROFILER_FILE_PATH contains "true")
      --enable-progressive-syncs                  Progressive syncs; env PATROL_APPLICATIONSET_CONTROLLER_ENABLE_PROGRESSIVE_SYNCS, patrol-cmd-params-cm applicationsetcontroller.enable.progressive.syncs
      --enable-scm-providers                      Enable SCM provider and pull request generators; env PATROL_APPLICATIONSET_CONTROLLER_ENABLE_SCM_PROVIDERS, patrol-cmd-params-cm applicationsetcontroller.enable.scm.providers (default true)
      --enable-tokenref-strict-mode               Require scm-creds labelled tokenRef Secrets; env PATROL_APPLICATIONSET_CONTROLLER_TOKENREF_STRICT_MODE, patrol-cmd-params-cm applicationsetcontroller.enable.tokenref.strict.mode
      --healthz-addr string                       Separate listener serving /healthz, /readyz and /livez ("" or "-": the metrics listener only) (default ":8081")
  -h, --help                                      help for appset-controller
      --internal-token-file string                File holding the shared internal token (default $PATROL_INTERNAL_TOKEN_FILE or /app/config/internal-token/token)
      --kubeconfig string                         Path to a kubeconfig file (default: in-cluster configuration, else $KUBECONFIG or ~/.kube/config)
      --logformat string                          Log format (json or text, any case) (one of json, text); env PATROL_APPLICATIONSET_CONTROLLER_LOGFORMAT, patrol-cmd-params-cm applicationsetcontroller.log.format (default "json")
      --loglevel level                            Log level (a logrus level name, any case); env PATROL_APPLICATIONSET_CONTROLLER_LOGLEVEL, patrol-cmd-params-cm applicationsetcontroller.log.level (default info)
      --max-resources-status-count int            Maximum resources in ApplicationSet status; env PATROL_APPLICATIONSET_CONTROLLER_MAX_RESOURCES_STATUS_COUNT, patrol-cmd-params-cm applicationsetcontroller.status.max.resources.count (default 5000)
      --metrics-addr string                       Address of the metrics, probe and profiler listener ("-" disables it) (default ":8080")
      --metrics-applicationset-labels strings     ApplicationSet label keys exported by patrol_appset_labels
  -n, --namespace string                          Control-plane namespace (default: the pod namespace, else the kubeconfig context namespace)
      --policy string                             Application sync policy (one of "", sync, create-only, create-update, create-delete); env PATROL_APPLICATIONSET_CONTROLLER_POLICY, patrol-cmd-params-cm applicationsetcontroller.policy
      --preserved-annotations strings             Annotations preserved on generated Applications; env PATROL_APPLICATIONSET_CONTROLLER_GLOBAL_PRESERVED_ANNOTATIONS, patrol-cmd-params-cm applicationsetcontroller.global.preserved.annotations
      --preserved-labels strings                  Labels preserved on generated Applications; env PATROL_APPLICATIONSET_CONTROLLER_GLOBAL_PRESERVED_LABELS, patrol-cmd-params-cm applicationsetcontroller.global.preserved.labels
      --repo-server string                        Repo-server address; env PATROL_APPLICATIONSET_CONTROLLER_REPO_SERVER, patrol-cmd-params-cm repo.server (default "patrol-repo-server:8081")
      --repo-server-ca-cert-path string           CA bundle verifying the repo-server; env PATROL_APPLICATIONSET_CONTROLLER_REPO_SERVER_CA_CERT_PATH, patrol-cmd-params-cm applicationsetcontroller.repo.server.ca.cert.path
      --repo-server-client-cert-key-path string   mTLS client key; env PATROL_APPLICATIONSET_CONTROLLER_REPO_SERVER_CLIENT_CERT_KEY_PATH, patrol-cmd-params-cm applicationsetcontroller.repo.server.client.cert.key.path (default "/app/config/reposerver/mtls/client.key")
      --repo-server-client-cert-path string       mTLS client certificate; env PATROL_APPLICATIONSET_CONTROLLER_REPO_SERVER_CLIENT_CERT_PATH, patrol-cmd-params-cm applicationsetcontroller.repo.server.client.cert.path (default "/app/config/reposerver/mtls/client.crt")
      --repo-server-plaintext                     Connect to the repo-server without TLS; env PATROL_APPLICATIONSET_CONTROLLER_REPO_SERVER_PLAINTEXT, patrol-cmd-params-cm applicationsetcontroller.repo.server.plaintext
      --repo-server-timeout-seconds int           Repo-server request timeout in seconds; env PATROL_APPLICATIONSET_CONTROLLER_REPO_SERVER_TIMEOUT_SECONDS, patrol-cmd-params-cm applicationsetcontroller.repo.server.timeout.seconds (default 60)
      --scm-no-proxy string                       Hosts reached without the SCM proxy
      --scm-proxy-url string                      Proxy of SCM provider API requests
      --scm-root-ca-path string                   CA bundle for SCM providers; env PATROL_APPLICATIONSET_CONTROLLER_SCM_ROOT_CA_PATH, patrol-cmd-params-cm applicationsetcontroller.scm.root.ca.path
      --shutdown-timeout duration                 Bound of the graceful drain after SIGTERM (default 30s)
      --webhook-addr string                       Address receiving SCM webhooks (upstream :7000); patrol-server's /api/webhook refreshes ApplicationSets too
      --webhook-parallelism-limit int             Concurrent webhook requests; env PATROL_APPLICATIONSET_CONTROLLER_WEBHOOK_PARALLELISM_LIMIT, patrol-cmd-params-cm applicationsetcontroller.webhook.parallelism.limit (default 50)
```

## patrolcd notifications-controller

Notifications controller (triggers, templates, services).

```text
Run the notifications controller

Usage:
  patrolcd notifications-controller [flags]

Flags:
      --allow-private-egress                      Let control-plane notification services reach private, loopback and link-local addresses
      --app-label-selector string                 Label selector restricting the watched Applications
      --application-namespaces strings            Additional namespaces (globs) Applications may live in; env PATROL_APPLICATION_NAMESPACES, patrol-cmd-params-cm application.namespaces
      --config-map-name string                    Notifications ConfigMap (default "patrol-notifications-cm")
      --context string                            Kubeconfig context to use
      --egress-allowed-cidrs strings              Non-public CIDRs control-plane notification services may reach
      --enable-leader-election                    Run only on the replica holding the leader election Lease
      --enable-pprof                              Serve /debug/pprof/ on the metrics listener (otherwise only while the file named by $PATROL_ENABLE_PROFILER_FILE_PATH contains "true")
      --healthz-addr string                       Separate listener serving /healthz, /readyz and /livez ("" or "-": the metrics listener only)
  -h, --help                                      help for notifications-controller
      --internal-token-file string                File holding the shared internal token (default $PATROL_INTERNAL_TOKEN_FILE or /app/config/internal-token/token)
      --kubeconfig string                         Path to a kubeconfig file (default: in-cluster configuration, else $KUBECONFIG or ~/.kube/config)
      --logformat string                          Log format (json or text, any case) (one of json, text); env PATROL_NOTIFICATIONS_CONTROLLER_LOGFORMAT, patrol-cmd-params-cm notificationscontroller.log.format (default "json")
      --loglevel level                            Log level (a logrus level name, any case); env PATROL_NOTIFICATIONS_CONTROLLER_LOGLEVEL, patrol-cmd-params-cm notificationscontroller.log.level (default info)
      --metrics-addr string                       Address of the metrics, probe and profiler listener ("-" disables it) (default ":9001")
  -n, --namespace string                          Control-plane namespace (default: the pod namespace, else the kubeconfig context namespace)
      --processors-count int                      Notification workers; env PATROL_NOTIFICATION_CONTROLLER_PROCESSORS_COUNT, patrol-cmd-params-cm notificationscontroller.processors.count (default 1)
      --repo-server string                        Repo-server address of the repo.* template helpers (default "patrol-repo-server:8081")
      --repo-server-ca-cert-path string           CA bundle verifying the repo-server; env PATROL_NOTIFICATION_CONTROLLER_REPO_SERVER_CA_CERT_PATH, patrol-cmd-params-cm notificationscontroller.repo.server.ca.cert.path
      --repo-server-client-cert-key-path string   mTLS client key; env PATROL_NOTIFICATION_CONTROLLER_REPO_SERVER_CLIENT_CERT_KEY_PATH, patrol-cmd-params-cm notificationscontroller.repo.server.client.cert.key.path (default "/app/config/reposerver/mtls/client.key")
      --repo-server-client-cert-path string       mTLS client certificate; env PATROL_NOTIFICATION_CONTROLLER_REPO_SERVER_CLIENT_CERT_PATH, patrol-cmd-params-cm notificationscontroller.repo.server.client.cert.path (default "/app/config/reposerver/mtls/client.crt")
      --repo-server-plaintext                     Connect to the repo-server without TLS; env PATROL_NOTIFICATION_CONTROLLER_REPO_SERVER_PLAINTEXT, patrol-cmd-params-cm notificationscontroller.repo.server.plaintext
      --secret-name string                        Notifications Secret (default "patrol-notifications-secret")
      --self-service-notification-enabled         Namespace-level notification configuration; env PATROL_NOTIFICATION_CONTROLLER_SELF_SERVICE_NOTIFICATION_ENABLED, patrol-cmd-params-cm notificationscontroller.selfservice.enabled
      --shutdown-timeout duration                 Bound of the graceful drain after SIGTERM (default 30s)
      --tenant-egress-allowed-cidrs strings       Non-public CIDRs self-service (namespace) configurations may reach
```

## patrolcd cmp-server

Config management plugin sidecar server (runs in a sidecar container of the repo-server pod; unix socket).

```text
Run a config management plugin sidecar server

Usage:
  patrolcd cmp-server [flags]

Flags:
      --config-dir-path string      Directory of the config management plugin configuration file (plugin.yaml) (default "/home/patrol/cmp-server/config")
      --enable-pprof                Serve /debug/pprof/ on the metrics listener (otherwise only while the file named by $PATROL_ENABLE_PROFILER_FILE_PATH contains "true")
      --exec-timeout duration       Timeout of each plugin command (env PATROL_EXEC_TIMEOUT) (default 1m30s)
  -h, --help                        help for cmp-server
      --logformat string            Log format: json|text (env PATROL_CMP_SERVER_LOGFORMAT) (default "json")
      --loglevel string             Log level: debug|info|warn|error (env PATROL_CMP_SERVER_LOGLEVEL) (default "info")
      --max-archive-bytes int       Maximum compressed source archive size per request; negative disables (default 1073741824)
      --max-extracted-bytes int     Maximum extracted source size per request; negative disables (default 1073741824)
      --max-files int               Maximum number of archive entries per request; negative disables (default 500000)
      --metrics-addr string         Address of the metrics, probe and profiler listener ("-" disables it) (default "-")
      --pass-env strings            Sidecar environment variables passed to plugin commands: names, PREFIX* or * (env PATROL_CMP_PASS_ENV); plugins get only PATH, HOME, TMPDIR and LANG otherwise
      --receive-timeout duration    Maximum time to receive one request stream; 0 leaves only the client deadline (env PATROL_CMP_RECEIVE_TIMEOUT) (default 5m0s)
      --shutdown-timeout duration   Bound of the graceful drain after SIGTERM (default 30s)
      --socket-dir string           Plugin socket directory shared with the repo-server (env PATROL_PLUGINSOCKFILEPATH) (default "/home/patrol/cmp-server/plugins")
      --work-dir string             Parent of the _cmp_server work directory, wiped at startup (env PATROL_CMP_WORKDIR) (default "/tmp")
```

## patrolcd commit-server

Commit-server: writes hydrated manifests to git for the source hydrator; no Kubernetes access.

```text
Run the commit-server (source hydrator writes; no Kubernetes access)

Usage:
  patrolcd commit-server [flags]

Flags:
      --address string                Listen address; env PATROL_COMMIT_SERVER_LISTEN_ADDRESS, patrol-cmd-params-cm commitserver.listen.address (default "0.0.0.0")
      --credentials-dir string        Directory of per-command credential files (tmpfs recommended; default $TMPDIR)
      --enable-pprof                  Serve /debug/pprof/ on the metrics listener (otherwise only while the file named by $PATROL_ENABLE_PROFILER_FILE_PATH contains "true")
      --git-path string               git executable (default "git")
      --git-timeout duration          Timeout of each git command (0: the internal/git default)
      --healthz-addr string           Separate listener serving /healthz, /readyz and /livez ("" or "-": the metrics listener only)
  -h, --help                          help for commit-server
      --internal-token-file string    File holding the shared internal token (default $PATROL_INTERNAL_TOKEN_FILE or /app/config/internal-token/token)
      --logformat string              Log format (json or text, any case) (one of json, text); env PATROL_COMMIT_SERVER_LOGFORMAT, patrol-cmd-params-cm commitserver.log.format (default "json")
      --loglevel level                Log level (a logrus level name, any case); env PATROL_COMMIT_SERVER_LOGLEVEL, patrol-cmd-params-cm commitserver.log.level (default info)
      --max-concurrent-requests int   Requests processed at once (0: unlimited)
      --metrics-addr string           Address of the metrics, probe and profiler listener ("-" disables it) (default: --metrics-address and --metrics-port)
      --metrics-address string        Metrics listen address; env PATROL_COMMIT_SERVER_METRICS_LISTEN_ADDRESS, patrol-cmd-params-cm commitserver.metrics.listen.address (default "0.0.0.0")
      --metrics-port int              Port of the metrics listener (default 8087)
      --port int                      Port of the API listener (default 8086)
      --shutdown-timeout duration     Bound of the graceful drain after SIGTERM (default 30s)
      --ssh-known-hosts-path string   SSH known hosts file mounted from patrol-ssh-known-hosts-cm ("-" disables) (default "/app/config/ssh/ssh_known_hosts")
      --tls-cert string               TLS certificate file of the listener (reloaded when it changes)
      --tls-certs-dir string          Repository TLS certificates directory mounted from patrol-tls-certs-cm ("-" disables) (default "/app/config/tls")
      --tls-client-ca string          CA bundle requiring and verifying client certificates (mutual TLS)
      --tls-key string                TLS private key file of the listener
      --work-dir string               Directory of per-request clones (an ephemeral volume; default $TMPDIR)
```

## patrolcd bootstrap

First-run initialisation: the init container of the install manifests and the Helm chart. See [bootstrap.md](../architecture/components/bootstrap.md).

```text
Creates, in the control-plane namespace, what a fresh installation needs, and keeps existing
material (one exception: a stale initial admin Secret, below):
  - patrol-internal-token (key token): the shared token of the internal APIs
  - patrol-secret key server.secretkey: the session signing key
  - patrol-secret keys admin.password and admin.passwordMtime, and patrol-initial-admin-secret
    (key password): the initial admin password, unless patrol-cm sets admin.enabled to false
    or patrol-secret already holds admin.password. An initial admin Secret older than patrol-secret
    (left over from an earlier installation) is replaced by a new one
  - patrol-ssh-known-hosts-cm key ssh_known_hosts: the published host keys of GitHub, Bitbucket Cloud,
    GitLab.com and Azure DevOps, when the ConfigMap exists without that key (a present key is kept)

Runs are idempotent (a converged namespace is not written to) and safe to run concurrently. The
install manifests run it as an init container; it can also be run by hand with a kubeconfig.
Nothing secret is printed: read the initial password with `patrol admin initial-password`.
Exit status 0 when converged, 1 on failure.

Usage:
  patrolcd bootstrap [flags]

Flags:
      --context string         Kubeconfig context to use
  -h, --help                   help for bootstrap
      --kubeconfig string      Path to a kubeconfig file (default: in-cluster configuration, else $KUBECONFIG or ~/.kube/config)
      --logformat string       Log format: json|text (default: json in a pod, text elsewhere)
      --loglevel string        Log level: debug|info|warn|error (default "info")
  -n, --namespace string       Control-plane namespace (default: $POD_NAMESPACE, else the pod's service account namespace)
      --skip-admin             Do not create the initial admin password (admin.password, patrol-initial-admin-secret)
      --skip-internal-token    Do not create Secret patrol-internal-token
      --skip-ssh-known-hosts   Do not seed the default known hosts into ConfigMap patrol-ssh-known-hosts-cm
      --timeout duration       Bound of the whole run, retries of transient failures included (default 1m0s)
```

## patrolcd k8s-auth

Exec credential plugin printing a `client.authentication.k8s.io/v1beta1` ExecCredential for EKS (`aws`), GKE (`gcp`) and AKS (`azure`) clusters.

```text
Print Kubernetes exec credentials for managed cloud clusters

Usage:
  patrolcd k8s-auth [flags]
  patrolcd k8s-auth [command]

Available Commands:
  aws         Print an EKS token (presigned sts:GetCallerIdentity)
  azure       Print an AKS token (Microsoft Entra ID; workload identity by default)
  gcp         Print a GKE token from Google application default credentials

Flags:
  -h, --help   help for k8s-auth

Use "patrolcd k8s-auth [command] --help" for more information about a command.
```

## patrolcd version

Build information (`--short`: version only; `-o json`).

```text
Print version information

Usage:
  patrolcd version [flags]

Flags:
  -h, --help            help for version
  -o, --output string   Output format: json or wide
      --short           Print the version number only
```
