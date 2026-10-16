package settings

import "strings"

// paramDef is one patrol-cmd-params-cm key with the components (and upstream environment
// variables) that read it. The table follows the Argo CD v3.5.5 manifests
// (configMapKeyRef wiring) and binaries (flag defaults); see OPS-017..028.
type paramDef struct {
	key        string
	typ        ParamType
	def        string
	min, max   string
	allowed    []string
	foldCase   bool
	sep        string
	allowEmpty bool
	flag       string
	desc       string
	na         string
	deprecated string
	envs       []compEnv
}

type compEnv struct {
	c   Component
	env string // upstream environment variable ("" = not read from the environment)
}

func (d paramDef) spec(ce compEnv) ParamSpec {
	return ParamSpec{
		Key:             d.key,
		Component:       ce.c,
		Env:             PatrolEnv(ce.env),
		UpstreamEnv:     ce.env,
		Flag:            d.flag,
		Type:            d.typ,
		Default:         d.def,
		Min:             d.min,
		Max:             d.max,
		Allowed:         d.allowed,
		CaseInsensitive: d.foldCase,
		Separator:       d.sep,
		AllowEmpty:      d.allowEmpty,
		NotApplicable:   d.na,
		Deprecated:      d.deprecated,
		Description:     d.desc,
	}
}

// PatrolEnv maps an Argo CD environment variable to Patrol's: the ARGOCD_ prefix becomes
// PATROL_; other names (GRPC_*, REDIS*, DEX_*) are kept. One upstream inconsistency is
// fixed: the repo-server manifest projects reposerver.metrics.listen.address into
// ARGOCD_REPO_SERVER_LISTEN_METRICS_ADDRESS while the binary reads
// ARGOCD_REPO_SERVER_METRICS_LISTEN_ADDRESS; Patrol uses PATROL_REPO_SERVER_METRICS_LISTEN_ADDRESS
// for both.
func PatrolEnv(upstream string) string {
	if upstream == "ARGOCD_REPO_SERVER_LISTEN_METRICS_ADDRESS" {
		upstream = "ARGOCD_REPO_SERVER_METRICS_LISTEN_ADDRESS"
	}
	if rest, ok := strings.CutPrefix(upstream, "ARGOCD_"); ok {
		return "PATROL_" + rest
	}
	return upstream
}

const (
	cC = ComponentController
	cS = ComponentServer
	cR = ComponentRepoServer
	cA = ComponentAppSetController
	cN = ComponentNotificationsController
	cM = ComponentCommitServer
	cD = ComponentDex
)

func on(c Component, env string) compEnv { return compEnv{c: c, env: env} }

var (
	logFormats = []string{"json", "text"}
	tlsVers    = []string{"1.0", "1.1", "1.2", "1.3"}
)

const (
	naRedis = "Patrol CD does not use Redis (ADR-0004)"
	naGRPC  = "Patrol CD services use JSON over HTTP, not gRPC (ADR-0005, ADR-0006)"
	mtlsCrt = "/app/config/reposerver/mtls/client.crt"
	mtlsKey = "/app/config/reposerver/mtls/client.key"
)

var paramDefs = buildParamDefs()

func buildParamDefs() []paramDef {
	defs := []paramDef{
		// Shared keys (OPS-018).
		{key: "repo.server", typ: ParamString, def: "patrol-repo-server:8081", flag: "repo-server", desc: "repo-server address",
			envs: []compEnv{on(cC, "ARGOCD_APPLICATION_CONTROLLER_REPO_SERVER"), on(cS, "ARGOCD_SERVER_REPO_SERVER"), on(cA, "ARGOCD_APPLICATIONSET_CONTROLLER_REPO_SERVER")}},
		{key: "commit.server", typ: ParamString, def: "patrol-commit-server:8086", flag: "commit-server", desc: "commit-server address",
			envs: []compEnv{on(cC, "ARGOCD_APPLICATION_CONTROLLER_COMMIT_SERVER")}},
		{key: "redis.server", typ: ParamString, def: "", na: naRedis, desc: "Redis address",
			envs: []compEnv{on(cC, "REDIS_SERVER"), on(cS, "REDIS_SERVER"), on(cR, "REDIS_SERVER")}},
		{key: "redis.compression", typ: ParamString, def: "gzip", allowed: []string{"gzip", "none"}, na: naRedis, desc: "Redis value compression",
			envs: []compEnv{on(cC, "REDIS_COMPRESSION"), on(cS, "REDIS_COMPRESSION"), on(cR, "REDIS_COMPRESSION")}},
		{key: "redis.db", typ: ParamInt, def: "0", min: "0", na: naRedis, desc: "Redis database",
			envs: []compEnv{on(cC, "REDISDB"), on(cS, "REDISDB"), on(cR, "REDISDB")}},
		{key: "hydrator.enabled", typ: ParamBool, def: "false", flag: "hydrator-enabled", desc: "enable the source hydrator",
			envs: []compEnv{on(cC, "ARGOCD_HYDRATOR_ENABLED"), on(cS, "ARGOCD_HYDRATOR_ENABLED")}},
		{key: "otlp.address", typ: ParamString, def: "", flag: "otlp-address", desc: "OpenTelemetry collector address",
			envs: []compEnv{on(cC, "ARGOCD_APPLICATION_CONTROLLER_OTLP_ADDRESS"), on(cS, "ARGOCD_SERVER_OTLP_ADDRESS"), on(cR, "ARGOCD_REPO_SERVER_OTLP_ADDRESS")}},
		{key: "otlp.insecure", typ: ParamBool, def: "true", flag: "otlp-insecure", desc: "OpenTelemetry collector without TLS",
			envs: []compEnv{on(cC, "ARGOCD_APPLICATION_CONTROLLER_OTLP_INSECURE"), on(cS, "ARGOCD_SERVER_OTLP_INSECURE"), on(cR, "ARGOCD_REPO_SERVER_OTLP_INSECURE")}},
		{key: "otlp.headers", typ: ParamStringMap, def: "", sep: ",", flag: "otlp-headers", desc: "OpenTelemetry headers (k1=v1,k2=v2)",
			envs: []compEnv{on(cC, "ARGOCD_APPLICATION_CONTROLLER_OTLP_HEADERS"), on(cS, "ARGOCD_SERVER_OTLP_HEADERS"), on(cR, "ARGOCD_REPO_SERVER_OTLP_HEADERS")}},
		{key: "otlp.attrs", typ: ParamStringList, def: "", sep: ",", flag: "otlp-attrs", desc: "OpenTelemetry attributes (key:value,...)",
			envs: []compEnv{on(cC, "ARGOCD_APPLICATION_CONTROLLER_OTLP_ATTRS"), on(cS, "ARGOCD_SERVER_OTLP_ATTRS"), on(cR, "ARGOCD_REPO_SERVER_OTLP_ATTRS")}},
		{key: "application.namespaces", typ: ParamStringList, def: "", sep: ",", flag: "application-namespaces", desc: "additional namespaces (globs) Applications may live in",
			envs: []compEnv{on(cC, "ARGOCD_APPLICATION_NAMESPACES"), on(cS, "ARGOCD_APPLICATION_NAMESPACES"), on(cN, "ARGOCD_APPLICATION_NAMESPACES")}},
		{key: "log.format.timestamp", typ: ParamString, def: "", desc: `log timestamp layout ("" = RFC 3339)`,
			envs: []compEnv{on(cC, "ARGOCD_LOG_FORMAT_TIMESTAMP"), on(cR, "ARGOCD_LOG_FORMAT_TIMESTAMP"), on(cA, "ARGOCD_LOG_FORMAT_TIMESTAMP"), on(cM, "ARGOCD_LOG_FORMAT_TIMESTAMP"), on(cD, "ARGOCD_LOG_FORMAT_TIMESTAMP"), on(cN, "ARGOCD_LOG_FORMAT_TIMESTAMP")}},

		// Application controller (OPS-019, OPS-020, OPS-028).
		{key: "controller.status.processors", typ: ParamInt, def: "20", min: "0", max: "2147483647", flag: "status-processors", desc: "application refresh workers", envs: []compEnv{on(cC, "ARGOCD_APPLICATION_CONTROLLER_STATUS_PROCESSORS")}},
		{key: "controller.operation.processors", typ: ParamInt, def: "10", min: "0", max: "2147483647", flag: "operation-processors", desc: "operation workers", envs: []compEnv{on(cC, "ARGOCD_APPLICATION_CONTROLLER_OPERATION_PROCESSORS")}},
		{key: "controller.hydration.processors", typ: ParamInt, def: "5", min: "1", max: "2147483647", flag: "hydration-processors", desc: "hydration workers", envs: []compEnv{on(cC, "ARGOCD_APPLICATION_CONTROLLER_HYDRATION_PROCESSORS")}},
		{key: "controller.metrics.cache.expiration", typ: ParamDuration, def: "0s", flag: "metrics-cache-expiration", desc: "Prometheus metrics cache expiration (0 disables)", envs: []compEnv{on(cC, "ARGOCD_APPLICATION_CONTROLLER_METRICS_CACHE_EXPIRATION")}},
		{key: "controller.self.heal.timeout.seconds", typ: ParamInt, def: "0", min: "0", max: "2147483647", flag: "self-heal-timeout-seconds", desc: "fixed delay between self-heal attempts", envs: []compEnv{on(cC, "ARGOCD_APPLICATION_CONTROLLER_SELF_HEAL_TIMEOUT_SECONDS")}},
		{key: "controller.self.heal.backoff.timeout.seconds", typ: ParamInt, def: "2", min: "0", max: "2147483647", flag: "self-heal-backoff-timeout-seconds", desc: "initial self-heal backoff (0 disables backoff)", envs: []compEnv{on(cC, "ARGOCD_APPLICATION_CONTROLLER_SELF_HEAL_BACKOFF_TIMEOUT_SECONDS")}},
		{key: "controller.self.heal.backoff.factor", typ: ParamInt, def: "3", min: "0", max: "2147483647", flag: "self-heal-backoff-factor", desc: "self-heal backoff factor", envs: []compEnv{on(cC, "ARGOCD_APPLICATION_CONTROLLER_SELF_HEAL_BACKOFF_FACTOR")}},
		{key: "controller.self.heal.backoff.cap.seconds", typ: ParamInt, def: "300", min: "0", max: "2147483647", flag: "self-heal-backoff-cap-seconds", desc: "maximum self-heal backoff", envs: []compEnv{on(cC, "ARGOCD_APPLICATION_CONTROLLER_SELF_HEAL_BACKOFF_CAP_SECONDS")}},
		{key: "controller.self.heal.backoff.cooldown.seconds", typ: ParamInt, def: "330", min: "0", max: "2147483647", flag: "self-heal-backoff-cooldown-seconds", deprecated: "has no effect (upstream deprecated)", desc: "self-heal backoff cooldown", envs: []compEnv{on(cC, "ARGOCD_APPLICATION_CONTROLLER_SELF_HEAL_BACKOFF_COOLDOWN_SECONDS")}},
		{key: "controller.sync.timeout.seconds", typ: ParamInt, def: "0", min: "0", max: "2147483647", flag: "sync-timeout", desc: "sync operation timeout (0 = none)", envs: []compEnv{on(cC, "ARGOCD_APPLICATION_CONTROLLER_SYNC_TIMEOUT")}},
		{key: "controller.sync.wave.delay.seconds", typ: ParamInt, def: "2", min: "0", desc: "delay between sync waves", envs: []compEnv{on(cC, "ARGOCD_SYNC_WAVE_DELAY")}},
		{key: "controller.app.state.cache.expiration", typ: ParamDuration, def: "1h", max: "10h", flag: "app-state-cache-expiration", desc: "application state cache TTL", envs: []compEnv{on(cC, "ARGOCD_APP_STATE_CACHE_EXPIRATION")}},
		{key: "controller.default.cache.expiration", typ: ParamDuration, def: "24h", flag: "default-cache-expiration", desc: "default cache TTL", envs: []compEnv{on(cC, "ARGOCD_DEFAULT_CACHE_EXPIRATION")}},
		{key: "controller.resource.health.persist", typ: ParamBool, def: "false", flag: "persist-resource-health", desc: "store resource health in Application status", envs: []compEnv{on(cC, "ARGOCD_APPLICATION_CONTROLLER_PERSIST_RESOURCE_HEALTH")}},
		{key: "controller.sharding.algorithm", typ: ParamString, def: "legacy", allowed: []string{"legacy", "round-robin", "consistent-hashing"}, flag: "sharding-method", desc: "cluster to shard assignment", envs: []compEnv{on(cC, "ARGOCD_CONTROLLER_SHARDING_ALGORITHM")}},
		{key: "controller.kubectl.parallelism.limit", typ: ParamInt, def: "20", min: "0", flag: "kubectl-parallelism-limit", desc: "concurrent apply operations (<1 = unlimited)", envs: []compEnv{on(cC, "ARGOCD_APPLICATION_CONTROLLER_KUBECTL_PARALLELISM_LIMIT")}},
		{key: "controller.repo.error.grace.period.seconds", typ: ParamDuration, def: "180s", flag: "repo-error-grace-period-seconds", desc: "grace period for repo-server errors (a duration such as 180s)", envs: []compEnv{on(cC, "ARGOCD_REPO_ERROR_GRACE_PERIOD_SECONDS")}},
		{key: "controller.diff.server.side", typ: ParamBool, def: "false", flag: "server-side-diff-enabled", desc: "use server-side diff by default", envs: []compEnv{on(cC, "ARGOCD_APPLICATION_CONTROLLER_SERVER_SIDE_DIFF")}},
		{key: "controller.ignore.normalizer.jq.timeout", typ: ParamDuration, def: "0s", flag: "ignore-normalizer-jq-execution-timeout-seconds", desc: "jq ignore-rule timeout (0 = normalizer default)", envs: []compEnv{on(cC, "ARGOCD_IGNORE_NORMALIZER_JQ_TIMEOUT")}},
		{key: "controller.cluster.cache.batch.events.processing", typ: ParamBool, def: "true", desc: "batch cluster cache events", envs: []compEnv{on(cC, "ARGOCD_CLUSTER_CACHE_BATCH_EVENTS_PROCESSING")}},
		{key: "controller.cluster.cache.events.processing.interval", typ: ParamDuration, def: "100ms", desc: "cluster cache event batching interval", envs: []compEnv{on(cC, "ARGOCD_CLUSTER_CACHE_EVENTS_PROCESSING_INTERVAL")}},

		// API server (OPS-022, OPS-028).
		{key: "server.listen.address", typ: ParamString, def: "0.0.0.0", flag: "address", desc: "listen address", envs: []compEnv{on(cS, "ARGOCD_SERVER_LISTEN_ADDRESS")}},
		{key: "server.metrics.listen.address", typ: ParamString, def: "0.0.0.0", flag: "metrics-address", desc: "metrics listen address", envs: []compEnv{on(cS, "ARGOCD_SERVER_METRICS_LISTEN_ADDRESS")}},
		{key: "server.insecure", typ: ParamBool, def: "false", flag: "insecure", desc: "serve without TLS", envs: []compEnv{on(cS, "ARGOCD_SERVER_INSECURE")}},
		{key: "server.basehref", typ: ParamString, def: "/", flag: "basehref", desc: "UI base href", envs: []compEnv{on(cS, "ARGOCD_SERVER_BASEHREF")}},
		{key: "server.rootpath", typ: ParamString, def: "", flag: "rootpath", desc: "path prefix behind a reverse proxy", envs: []compEnv{on(cS, "ARGOCD_SERVER_ROOTPATH")}},
		{key: "server.staticassets", typ: ParamString, def: "/shared/app", flag: "staticassets", desc: "additional static assets directory", envs: []compEnv{on(cS, "ARGOCD_SERVER_STATIC_ASSETS")}},
		{key: "server.api.content.types", typ: ParamString, def: "application/json", allowEmpty: true, flag: "api-content-types", desc: "semicolon-separated content types allowed on non-GET requests (empty allows all)", envs: []compEnv{on(cS, "ARGOCD_API_CONTENT_TYPES")}},
		{key: "server.webhook.parallelism.limit", typ: ParamInt, def: "50", min: "1", max: "1000", flag: "webhook-parallelism-limit", desc: "concurrent webhook requests", envs: []compEnv{on(cS, "ARGOCD_SERVER_WEBHOOK_PARALLELISM_LIMIT")}},
		{key: "server.glob.cache.size", typ: ParamInt, def: "10000", min: "1", max: "2147483647", flag: "glob-cache-size", desc: "RBAC glob cache size (no effect: patterns are compiled with the policy)", envs: []compEnv{on(cS, "ARGOCD_SERVER_GLOB_CACHE_SIZE")}},
		{key: "server.webhook.refresh.workers", typ: ParamInt, def: "20", min: "1", max: "1000", flag: "webhook-refresh-workers", desc: "webhook refresh workers", envs: []compEnv{on(cS, "ARGOCD_SERVER_WEBHOOK_REFRESH_WORKERS")}},
		{key: "server.sync.replace.allowed", typ: ParamBool, def: "true", flag: "sync-with-replace-allowed", desc: "allow Replace syncs through the API", envs: []compEnv{on(cS, "ARGOCD_SYNC_WITH_REPLACE_ALLOWED")}},
		{key: "server.dex.server", typ: ParamString, def: "patrol-dex-server:5556", flag: "dex-server", desc: "Dex address", envs: []compEnv{on(cS, "ARGOCD_SERVER_DEX_SERVER")}},
		{key: "server.dex.server.plaintext", typ: ParamBool, def: "false", flag: "dex-server-plaintext", desc: "connect to Dex without TLS", envs: []compEnv{on(cS, "ARGOCD_SERVER_DEX_SERVER_PLAINTEXT")}},
		{key: "server.dex.server.strict.tls", typ: ParamBool, def: "false", flag: "dex-server-strict-tls", desc: "verify Dex's TLS certificate", envs: []compEnv{on(cS, "ARGOCD_SERVER_DEX_SERVER_STRICT_TLS")}},
		{key: "server.disable.auth", typ: ParamBool, def: "false", flag: "disable-auth", desc: "disable authentication and authorization", envs: []compEnv{on(cS, "ARGOCD_SERVER_DISABLE_AUTH")}},
		{key: "server.enable.gzip", typ: ParamBool, def: "true", flag: "enable-gzip", desc: "gzip HTTP responses", envs: []compEnv{on(cS, "ARGOCD_SERVER_ENABLE_GZIP")}},
		{key: "server.x.frame.options", typ: ParamString, def: "sameorigin", allowEmpty: true, flag: "x-frame-options", desc: "X-Frame-Options header (empty disables)", envs: []compEnv{on(cS, "ARGOCD_SERVER_X_FRAME_OPTIONS")}},
		{key: "server.content.security.policy", typ: ParamString, def: "frame-ancestors 'self';", allowEmpty: true, flag: "content-security-policy", desc: "Content-Security-Policy header (empty disables)", envs: []compEnv{on(cS, "ARGOCD_SERVER_CONTENT_SECURITY_POLICY")}},
		{key: "server.tls.minversion", typ: ParamString, def: "1.2", allowed: tlsVers, flag: "tlsminversion", desc: "minimum TLS version", envs: []compEnv{on(cS, "ARGOCD_TLS_MIN_VERSION")}},
		{key: "server.tls.maxversion", typ: ParamString, def: "1.3", allowed: tlsVers, flag: "tlsmaxversion", desc: "maximum TLS version", envs: []compEnv{on(cS, "ARGOCD_TLS_MAX_VERSION")}},
		{key: "server.tls.ciphers", typ: ParamString, def: "TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384", flag: "tlsciphers", desc: "colon-separated TLS cipher suites", envs: []compEnv{on(cS, "ARGOCD_TLS_CIPHERS")}},
		{key: "server.connection.status.cache.expiration", typ: ParamDuration, def: "1h", flag: "connection-status-cache-expiration", desc: "repository/cluster connection status cache TTL", envs: []compEnv{on(cS, "ARGOCD_SERVER_CONNECTION_STATUS_CACHE_EXPIRATION")}},
		{key: "server.oidc.cache.expiration", typ: ParamDuration, def: "3m", flag: "oidc-cache-expiration", desc: "OIDC state cache TTL", envs: []compEnv{on(cS, "ARGOCD_SERVER_OIDC_CACHE_EXPIRATION")}},
		{key: "server.app.state.cache.expiration", typ: ParamDuration, def: "1h", max: "10h", flag: "app-state-cache-expiration", desc: "application state cache TTL", envs: []compEnv{on(cS, "ARGOCD_APP_STATE_CACHE_EXPIRATION")}},
		{key: "server.default.cache.expiration", typ: ParamDuration, def: "24h", flag: "default-cache-expiration", desc: "default cache TTL", envs: []compEnv{on(cS, "ARGOCD_DEFAULT_CACHE_EXPIRATION")}},
		{key: "server.enable.proxy.extension", typ: ParamBool, def: "false", flag: "enable-proxy-extension", desc: "enable proxy extensions", envs: []compEnv{on(cS, "ARGOCD_SERVER_ENABLE_PROXY_EXTENSION")}},
		{key: "server.http.cookie.maxnumber", typ: ParamInt, def: "20", min: "1", desc: "maximum cookie chunks of the session token", envs: []compEnv{on(cS, "ARGOCD_MAX_COOKIE_NUMBER")}},

		// Repo-server (OPS-023, OPS-028).
		{key: "reposerver.listen.address", typ: ParamString, def: "0.0.0.0", flag: "address", desc: "listen address", envs: []compEnv{on(cR, "ARGOCD_REPO_SERVER_LISTEN_ADDRESS")}},
		{key: "reposerver.metrics.listen.address", typ: ParamString, def: "0.0.0.0", flag: "metrics-address", desc: "metrics listen address", envs: []compEnv{on(cR, "ARGOCD_REPO_SERVER_LISTEN_METRICS_ADDRESS")}},
		{key: "reposerver.parallelism.limit", typ: ParamInt, def: "0", min: "0", max: "2147483647", flag: "parallelismlimit", desc: "concurrent manifest generations (0 = unlimited)", envs: []compEnv{on(cR, "ARGOCD_REPO_SERVER_PARALLELISM_LIMIT")}},
		{key: "reposerver.disable.tls", typ: ParamBool, def: "false", flag: "disable-tls", desc: "serve without TLS", envs: []compEnv{on(cR, "ARGOCD_REPO_SERVER_DISABLE_TLS")}},
		{key: "reposerver.tls.minversion", typ: ParamString, def: "1.2", allowed: tlsVers, flag: "tlsminversion", desc: "minimum TLS version", envs: []compEnv{on(cR, "ARGOCD_TLS_MIN_VERSION")}},
		{key: "reposerver.tls.maxversion", typ: ParamString, def: "1.3", allowed: tlsVers, flag: "tlsmaxversion", desc: "maximum TLS version", envs: []compEnv{on(cR, "ARGOCD_TLS_MAX_VERSION")}},
		{key: "reposerver.tls.ciphers", typ: ParamString, def: "TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384", flag: "tlsciphers", desc: "colon-separated TLS cipher suites", envs: []compEnv{on(cR, "ARGOCD_TLS_CIPHERS")}},
		{key: "reposerver.client.ca.path", typ: ParamString, def: "/app/config/reposerver/mtls/client-ca.crt", allowEmpty: true, flag: "client-ca-path", desc: "client CA bundle enabling mTLS", envs: []compEnv{on(cR, "ARGOCD_REPO_SERVER_CLIENT_CA_PATH")}},
		{key: "reposerver.repo.cache.expiration", typ: ParamDuration, def: "24h", flag: "repo-cache-expiration", desc: "render cache TTL", envs: []compEnv{on(cR, "ARGOCD_REPO_CACHE_EXPIRATION")}},
		{key: "reposerver.default.cache.expiration", typ: ParamDuration, def: "24h", flag: "default-cache-expiration", desc: "default cache TTL", envs: []compEnv{on(cR, "ARGOCD_DEFAULT_CACHE_EXPIRATION")}},
		{key: "reposerver.max.combined.directory.manifests.size", typ: ParamQuantity, def: "10M", flag: "max-combined-directory-manifests-size", desc: "maximum combined manifest size of a directory source", envs: []compEnv{on(cR, "ARGOCD_REPO_SERVER_MAX_COMBINED_DIRECTORY_MANIFESTS_SIZE")}},
		{key: "reposerver.plugin.tar.exclusions", typ: ParamStringList, def: "", sep: ";", flag: "plugin-tar-exclude", desc: "semicolon-separated globs excluded from plugin tarballs", envs: []compEnv{on(cR, "ARGOCD_REPO_SERVER_PLUGIN_TAR_EXCLUSIONS")}},
		{key: "reposerver.plugin.use.manifest.generate.paths", typ: ParamBool, def: "false", flag: "plugin-use-manifest-generate-paths", desc: "send only manifest-generate-paths to plugins", envs: []compEnv{on(cR, "ARGOCD_REPO_SERVER_PLUGIN_USE_MANIFEST_GENERATE_PATHS")}},
		{key: "reposerver.allow.oob.symlinks", typ: ParamBool, def: "false", flag: "allow-oob-symlinks", desc: "allow symlinks leaving the repository", envs: []compEnv{on(cR, "ARGOCD_REPO_SERVER_ALLOW_OUT_OF_BOUNDS_SYMLINKS")}},
		{key: "reposerver.streamed.manifest.max.tar.size", typ: ParamQuantity, def: "100M", flag: "streamed-manifest-max-tar-size", desc: "maximum streamed manifest tarball", envs: []compEnv{on(cR, "ARGOCD_REPO_SERVER_STREAMED_MANIFEST_MAX_TAR_SIZE")}},
		{key: "reposerver.streamed.manifest.max.extracted.size", typ: ParamQuantity, def: "1G", flag: "streamed-manifest-max-extracted-size", desc: "maximum extracted streamed manifests", envs: []compEnv{on(cR, "ARGOCD_REPO_SERVER_STREAMED_MANIFEST_MAX_EXTRACTED_SIZE")}},
		{key: "reposerver.helm.manifest.max.extracted.size", typ: ParamQuantity, def: "1G", flag: "helm-manifest-max-extracted-size", desc: "maximum extracted Helm chart size", envs: []compEnv{on(cR, "ARGOCD_REPO_SERVER_HELM_MANIFEST_MAX_EXTRACTED_SIZE")}},
		{key: "reposerver.disable.helm.manifest.max.extracted.size", typ: ParamBool, def: "false", flag: "disable-helm-manifest-max-extracted-size", desc: "disable the Helm extraction limit", envs: []compEnv{on(cR, "ARGOCD_REPO_SERVER_DISABLE_HELM_MANIFEST_MAX_EXTRACTED_SIZE")}},
		{key: "reposerver.oci.manifest.max.extracted.size", typ: ParamQuantity, def: "1G", flag: "oci-manifest-max-extracted-size", desc: "maximum extracted OCI artifact size", envs: []compEnv{on(cR, "ARGOCD_REPO_SERVER_OCI_MANIFEST_MAX_EXTRACTED_SIZE")}},
		{key: "reposerver.disable.oci.manifest.max.extracted.size", typ: ParamBool, def: "false", flag: "disable-oci-manifest-max-extracted-size", desc: "disable the OCI extraction limit", envs: []compEnv{on(cR, "ARGOCD_REPO_SERVER_DISABLE_OCI_MANIFEST_MAX_EXTRACTED_SIZE")}},
		{key: "reposerver.oci.layer.media.types", typ: ParamStringList, def: "application/vnd.oci.image.layer.v1.tar,application/vnd.oci.image.layer.v1.tar+gzip,application/vnd.cncf.helm.chart.content.v1.tar+gzip", sep: ",", flag: "oci-layer-media-types", desc: "allowed OCI layer media types", envs: []compEnv{on(cR, "ARGOCD_REPO_SERVER_OCI_LAYER_MEDIA_TYPES")}},
		{key: "reposerver.revision.cache.lock.timeout", typ: ParamDuration, def: "10s", flag: "revision-cache-lock-timeout", desc: "revision cache lock timeout", envs: []compEnv{on(cR, "ARGOCD_REVISION_CACHE_LOCK_TIMEOUT")}},
		{key: "reposerver.enable.git.submodule", typ: ParamBool, def: "true", desc: "check out git submodules", envs: []compEnv{on(cR, "ARGOCD_GIT_MODULES_ENABLED")}},
		{key: "reposerver.git.lsremote.parallelism.limit", typ: ParamInt, def: "0", min: "0", max: "2147483647", desc: "concurrent git ls-remote calls (0 = unlimited)", envs: []compEnv{on(cR, "ARGOCD_GIT_LS_REMOTE_PARALLELISM_LIMIT")}},
		{key: "reposerver.git.request.timeout", typ: ParamDuration, def: "15s", desc: "git HTTP request timeout", envs: []compEnv{on(cR, "ARGOCD_GIT_REQUEST_TIMEOUT")}},
		{key: "reposerver.enable.builtin.git.config", typ: ParamBool, def: "true", flag: "enable-builtin-git-config", desc: "apply built-in git configuration", envs: []compEnv{on(cR, "ARGOCD_REPO_SERVER_ENABLE_BUILTIN_GIT_CONFIG")}},
		{key: "reposerver.include.hidden.directories", typ: ParamBool, def: "false", flag: "include-hidden-directories", desc: "include hidden directories in directory listings", envs: []compEnv{on(cR, "ARGOCD_REPO_SERVER_INCLUDE_HIDDEN_DIRECTORIES")}},
		{key: "reposerver.grpc.max.size", typ: ParamInt, def: "100", min: "0", max: "2147483647", desc: "maximum repo-server request/response size in MiB (upstream: gRPC message size)", envs: []compEnv{on(cR, "ARGOCD_GRPC_MAX_SIZE_MB")}},
		{key: "reposerver.helm.user.agent", typ: ParamString, def: "", desc: "User-Agent of Helm repository requests", envs: []compEnv{on(cR, "ARGOCD_HELM_USER_AGENT")}},

		// Commit server (OPS-024).
		{key: "commitserver.listen.address", typ: ParamString, def: "0.0.0.0", flag: "address", desc: "listen address", envs: []compEnv{on(cM, "ARGOCD_COMMIT_SERVER_LISTEN_ADDRESS")}},
		{key: "commitserver.metrics.listen.address", typ: ParamString, def: "0.0.0.0", flag: "metrics-address", desc: "metrics listen address", envs: []compEnv{on(cM, "ARGOCD_COMMIT_SERVER_METRICS_LISTEN_ADDRESS")}},

		// Dex (OPS-025).
		{key: "dexserver.disable.tls", typ: ParamBool, def: "false", flag: "disable-tls", desc: "serve Dex without TLS", envs: []compEnv{on(cD, "ARGOCD_DEX_SERVER_DISABLE_TLS")}},
		{key: "dexserver.connector.failure.continue", typ: ParamBool, def: "false", desc: "keep Dex running when a connector fails (read by Dex itself)", envs: []compEnv{on(cD, "DEX_CONTINUE_ON_CONNECTOR_FAILURE")}},

		// ApplicationSet controller (OPS-026, OPS-028).
		{key: "applicationsetcontroller.enable.leader.election", typ: ParamBool, def: "false", flag: "enable-leader-election", desc: "leader election", envs: []compEnv{on(cA, "ARGOCD_APPLICATIONSET_CONTROLLER_ENABLE_LEADER_ELECTION")}},
		{key: "applicationsetcontroller.policy", typ: ParamString, def: "", allowed: []string{"", "sync", "create-only", "create-update", "create-delete"}, flag: "policy", desc: "Application sync policy", envs: []compEnv{on(cA, "ARGOCD_APPLICATIONSET_CONTROLLER_POLICY")}},
		{key: "applicationsetcontroller.enable.policy.override", typ: ParamBool, def: "false", flag: "enable-policy-override", desc: "allow ApplicationSets to override the policy (unset: true when policy is empty)", envs: []compEnv{on(cA, "ARGOCD_APPLICATIONSET_CONTROLLER_ENABLE_POLICY_OVERRIDE")}},
		{key: "applicationsetcontroller.concurrent.reconciliations.max", typ: ParamInt, def: "10", min: "1", flag: "concurrent-reconciliations", desc: "concurrent reconciliations", envs: []compEnv{on(cA, "ARGOCD_APPLICATIONSET_CONTROLLER_CONCURRENT_RECONCILIATIONS")}},
		{key: "applicationsetcontroller.enable.new.git.file.globbing", typ: ParamBool, def: "false", flag: "enable-new-git-file-globbing", desc: "new git files generator globbing",
			envs: []compEnv{on(cA, "ARGOCD_APPLICATIONSET_CONTROLLER_ENABLE_NEW_GIT_FILE_GLOBBING"), on(cS, "ARGOCD_APPLICATIONSET_CONTROLLER_ENABLE_NEW_GIT_FILE_GLOBBING")}},
		{key: "applicationsetcontroller.debug", typ: ParamBool, def: "false", flag: "debug", desc: "debug logging (overrides log level)", envs: []compEnv{on(cA, "ARGOCD_APPLICATIONSET_CONTROLLER_DEBUG")}},
		{key: "applicationsetcontroller.dryrun", typ: ParamBool, def: "false", flag: "dry-run", desc: "do not change Applications", envs: []compEnv{on(cA, "ARGOCD_APPLICATIONSET_CONTROLLER_DRY_RUN")}},
		{key: "applicationsetcontroller.enable.git.submodule", typ: ParamBool, def: "true", desc: "check out git submodules", envs: []compEnv{on(cA, "ARGOCD_GIT_MODULES_ENABLED")}},
		{key: "applicationsetcontroller.enable.progressive.syncs", typ: ParamBool, def: "false", flag: "enable-progressive-syncs", desc: "progressive syncs", envs: []compEnv{on(cA, "ARGOCD_APPLICATIONSET_CONTROLLER_ENABLE_PROGRESSIVE_SYNCS")}},
		{key: "applicationsetcontroller.namespaces", typ: ParamStringList, def: "", sep: ",", flag: "applicationset-namespaces", desc: "namespaces (globs) watched for ApplicationSets", envs: []compEnv{on(cA, "ARGOCD_APPLICATIONSET_CONTROLLER_NAMESPACES")}},
		{key: "applicationsetcontroller.scm.root.ca.path", typ: ParamString, def: "", flag: "scm-root-ca-path", desc: "CA bundle for SCM providers",
			envs: []compEnv{on(cA, "ARGOCD_APPLICATIONSET_CONTROLLER_SCM_ROOT_CA_PATH"), on(cS, "ARGOCD_APPLICATIONSET_CONTROLLER_SCM_ROOT_CA_PATH")}},
		{key: "applicationsetcontroller.allowed.scm.providers", typ: ParamStringList, def: "", sep: ",", flag: "allowed-scm-providers", desc: "allowed SCM provider API URLs",
			envs: []compEnv{on(cA, "ARGOCD_APPLICATIONSET_CONTROLLER_ALLOWED_SCM_PROVIDERS"), on(cS, "ARGOCD_APPLICATIONSET_CONTROLLER_ALLOWED_SCM_PROVIDERS")}},
		{key: "applicationsetcontroller.enable.scm.providers", typ: ParamBool, def: "true", flag: "enable-scm-providers", desc: "enable SCM provider and pull request generators",
			envs: []compEnv{on(cA, "ARGOCD_APPLICATIONSET_CONTROLLER_ENABLE_SCM_PROVIDERS"), on(cS, "ARGOCD_APPLICATIONSET_CONTROLLER_ENABLE_SCM_PROVIDERS")}},
		{key: "applicationsetcontroller.enable.github.api.metrics", typ: ParamBool, def: "false", flag: "enable-github-api-metrics", desc: "GitHub API metrics",
			envs: []compEnv{on(cA, "ARGOCD_APPLICATIONSET_CONTROLLER_ENABLE_GITHUB_API_METRICS"), on(cS, "ARGOCD_APPLICATIONSET_CONTROLLER_ENABLE_GITHUB_API_METRICS")}},
		{key: "applicationsetcontroller.webhook.parallelism.limit", typ: ParamInt, def: "50", min: "1", max: "1000", flag: "webhook-parallelism-limit", desc: "concurrent webhook requests", envs: []compEnv{on(cA, "ARGOCD_APPLICATIONSET_CONTROLLER_WEBHOOK_PARALLELISM_LIMIT")}},
		{key: "applicationsetcontroller.requeue.after", typ: ParamDuration, def: "3m", min: "1s", max: "8760h", desc: "default requeue interval", envs: []compEnv{on(cA, "ARGOCD_APPLICATIONSET_CONTROLLER_REQUEUE_AFTER")}},
		{key: "applicationsetcontroller.enable.tokenref.strict.mode", typ: ParamBool, def: "false", flag: "enable-tokenref-strict-mode", desc: "require scm-creds labelled tokenRef Secrets", envs: []compEnv{on(cA, "ARGOCD_APPLICATIONSET_CONTROLLER_TOKENREF_STRICT_MODE")}},
		{key: "applicationsetcontroller.global.preserved.annotations", typ: ParamStringList, def: "", sep: ",", flag: "preserved-annotations", desc: "annotations preserved on generated Applications", envs: []compEnv{on(cA, "ARGOCD_APPLICATIONSET_CONTROLLER_GLOBAL_PRESERVED_ANNOTATIONS")}},
		{key: "applicationsetcontroller.global.preserved.labels", typ: ParamStringList, def: "", sep: ",", flag: "preserved-labels", desc: "labels preserved on generated Applications", envs: []compEnv{on(cA, "ARGOCD_APPLICATIONSET_CONTROLLER_GLOBAL_PRESERVED_LABELS")}},
		{key: "applicationsetcontroller.status.max.resources.count", typ: ParamInt, def: "5000", min: "0", flag: "max-resources-status-count", desc: "maximum resources in ApplicationSet status", envs: []compEnv{on(cA, "ARGOCD_APPLICATIONSET_CONTROLLER_MAX_RESOURCES_STATUS_COUNT")}},

		// Notifications controller (OPS-027).
		{key: "notificationscontroller.processors.count", typ: ParamInt, def: "1", min: "1", max: "2147483647", flag: "processors-count", desc: "notification workers", envs: []compEnv{on(cN, "ARGOCD_NOTIFICATION_CONTROLLER_PROCESSORS_COUNT")}},
		{key: "notificationscontroller.selfservice.enabled", typ: ParamBool, def: "false", flag: "self-service-notification-enabled", desc: "namespace-level notification configuration", envs: []compEnv{on(cN, "ARGOCD_NOTIFICATION_CONTROLLER_SELF_SERVICE_NOTIFICATION_ENABLED")}},
	}

	// Per-component log keys.
	for _, l := range []struct {
		prefix    string
		c         Component
		fmtEnv    string
		levelEnv  string
		fmtFlag   string
		levelFlag string
	}{
		{"controller", cC, "ARGOCD_APPLICATION_CONTROLLER_LOGFORMAT", "ARGOCD_APPLICATION_CONTROLLER_LOGLEVEL", "logformat", "loglevel"},
		{"server", cS, "ARGOCD_SERVER_LOGFORMAT", "ARGOCD_SERVER_LOG_LEVEL", "logformat", "loglevel"},
		{"reposerver", cR, "ARGOCD_REPO_SERVER_LOGFORMAT", "ARGOCD_REPO_SERVER_LOGLEVEL", "logformat", "loglevel"},
		{"commitserver", cM, "ARGOCD_COMMIT_SERVER_LOGFORMAT", "ARGOCD_COMMIT_SERVER_LOGLEVEL", "logformat", "loglevel"},
		{"dexserver", cD, "ARGOCD_DEX_SERVER_LOGFORMAT", "ARGOCD_DEX_SERVER_LOGLEVEL", "logformat", "loglevel"},
		{"applicationsetcontroller", cA, "ARGOCD_APPLICATIONSET_CONTROLLER_LOGFORMAT", "ARGOCD_APPLICATIONSET_CONTROLLER_LOGLEVEL", "logformat", "loglevel"},
		{"notificationscontroller", cN, "ARGOCD_NOTIFICATIONS_CONTROLLER_LOGFORMAT", "ARGOCD_NOTIFICATIONS_CONTROLLER_LOGLEVEL", "logformat", "loglevel"},
	} {
		defs = append(defs,
			paramDef{key: l.prefix + ".log.format", typ: ParamString, def: "json", allowed: logFormats, foldCase: true, flag: l.fmtFlag, desc: "log format (json or text, any case)", envs: []compEnv{on(l.c, l.fmtEnv)}},
			paramDef{key: l.prefix + ".log.level", typ: ParamLogLevel, def: "info", allowed: logLevelNames, flag: l.levelFlag, desc: "log level (a logrus level name, any case)", envs: []compEnv{on(l.c, l.levelEnv)}},
		)
	}

	// Kubernetes client tuning (OPS-021).
	for _, k := range []struct {
		prefix string
		c      Component
	}{{"controller", cC}, {"server", cS}, {"applicationsetcontroller", cA}} {
		defs = append(defs,
			paramDef{key: k.prefix + ".k8s.client.qps", typ: ParamFloat, def: "50", min: "0", desc: "Kubernetes client QPS", envs: []compEnv{on(k.c, "ARGOCD_K8S_CLIENT_QPS")}},
			paramDef{key: k.prefix + ".k8s.client.burst", typ: ParamInt, def: "100", min: "0", desc: "Kubernetes client burst (default 2x QPS)", envs: []compEnv{on(k.c, "ARGOCD_K8S_CLIENT_BURST")}},
			paramDef{key: k.prefix + ".k8s.client.max.idle.connections", typ: ParamInt, def: "500", min: "0", desc: "Kubernetes client idle connections", envs: []compEnv{on(k.c, "ARGOCD_K8S_CLIENT_MAX_IDLE_CONNECTIONS")}},
			paramDef{key: k.prefix + ".k8s.tcp.timeout", typ: ParamDuration, def: "30s", desc: "Kubernetes client dial timeout", envs: []compEnv{on(k.c, "ARGOCD_K8S_TCP_TIMEOUT")}},
			paramDef{key: k.prefix + ".k8s.tcp.keepalive", typ: ParamDuration, def: "30s", desc: "Kubernetes client TCP keep-alive", envs: []compEnv{on(k.c, "ARGOCD_K8S_TCP_KEEPALIVE")}},
			paramDef{key: k.prefix + ".k8s.tls.handshake.timeout", typ: ParamDuration, def: "10s", desc: "Kubernetes client TLS handshake timeout", envs: []compEnv{on(k.c, "ARGOCD_K8S_TLS_HANDSHAKE_TIMEOUT")}},
			paramDef{key: k.prefix + ".k8s.tcp.idle.timeout", typ: ParamDuration, def: "5m", desc: "Kubernetes client idle connection timeout", envs: []compEnv{on(k.c, "ARGOCD_K8S_TCP_IDLE_TIMEOUT")}},
		)
	}
	for _, k := range []struct {
		prefix string
		c      Component
	}{{"controller", cC}, {"server", cS}} {
		defs = append(defs,
			paramDef{key: k.prefix + ".k8sclient.retry.max", typ: ParamInt, def: "0", min: "0", max: "10", desc: "Kubernetes client retries", envs: []compEnv{on(k.c, "ARGOCD_K8SCLIENT_RETRY_MAX")}},
			paramDef{key: k.prefix + ".k8sclient.retry.base.backoff", typ: ParamInt, def: "100", min: "0", max: "1000", desc: "Kubernetes client retry base backoff in ms", envs: []compEnv{on(k.c, "ARGOCD_K8SCLIENT_RETRY_BASE_BACKOFF")}},
		)
	}
	for _, k := range []struct {
		prefix string
		c      Component
	}{{"controller", cC}, {"server", cS}, {"reposerver", cR}, {"commitserver", cM}, {"applicationsetcontroller", cA}} {
		defs = append(defs, paramDef{key: k.prefix + ".grpc.enable.txt.service.config", typ: ParamBool, def: "false", na: naGRPC, desc: "gRPC DNS TXT service config lookups", envs: []compEnv{on(k.c, "GRPC_ENABLE_TXT_SERVICE_CONFIG")}})
	}
	for _, k := range []struct {
		prefix string
		c      Component
	}{{"controller", cC}, {"server", cS}, {"reposerver", cR}, {"applicationsetcontroller", cA}} {
		defs = append(defs, paramDef{key: k.prefix + ".profile.enabled", typ: ParamBool, def: "false", desc: "pprof endpoints on the metrics port (projected as a file, read per request)", envs: []compEnv{on(k.c, "")}})
	}

	// Repo-server client settings per component (OPS-020, OPS-101/103).
	for _, r := range []struct {
		prefix    string
		c         Component
		envPrefix string
		timeout   bool
		plaintext bool
		strict    bool
	}{
		{"controller", cC, "ARGOCD_APPLICATION_CONTROLLER_REPO_SERVER", true, true, true},
		{"server", cS, "ARGOCD_SERVER_REPO_SERVER", true, true, true},
		{"applicationsetcontroller", cA, "ARGOCD_APPLICATIONSET_CONTROLLER_REPO_SERVER", true, true, true},
		{"notificationscontroller", cN, "ARGOCD_NOTIFICATION_CONTROLLER_REPO_SERVER", false, true, false},
	} {
		p := r.prefix + ".repo.server."
		if r.timeout {
			defs = append(defs, paramDef{key: p + "timeout.seconds", typ: ParamInt, def: "60", min: "0", flag: "repo-server-timeout-seconds", desc: "repo-server request timeout in seconds", envs: []compEnv{on(r.c, r.envPrefix+"_TIMEOUT_SECONDS")}})
		}
		if r.plaintext {
			defs = append(defs, paramDef{key: p + "plaintext", typ: ParamBool, def: "false", flag: "repo-server-plaintext", desc: "connect to the repo-server without TLS", envs: []compEnv{on(r.c, r.envPrefix+"_PLAINTEXT")}})
		}
		if r.strict {
			defs = append(defs, paramDef{key: p + "strict.tls", typ: ParamBool, def: "false", flag: "repo-server-strict-tls", deprecated: "use repo.server.ca.cert.path", desc: "verify the repo-server certificate", envs: []compEnv{on(r.c, r.envPrefix+"_STRICT_TLS")}})
		}
		defs = append(defs,
			paramDef{key: p + "ca.cert.path", typ: ParamString, def: "", flag: "repo-server-ca-cert-path", desc: "CA bundle verifying the repo-server", envs: []compEnv{on(r.c, r.envPrefix+"_CA_CERT_PATH")}},
			paramDef{key: p + "client.cert.path", typ: ParamString, def: mtlsCrt, flag: "repo-server-client-cert-path", desc: "mTLS client certificate", envs: []compEnv{on(r.c, r.envPrefix+"_CLIENT_CERT_PATH")}},
			paramDef{key: p + "client.cert.key.path", typ: ParamString, def: mtlsKey, flag: "repo-server-client-cert-key-path", desc: "mTLS client key", envs: []compEnv{on(r.c, r.envPrefix+"_CLIENT_CERT_KEY_PATH")}},
		)
	}

	return defs
}
