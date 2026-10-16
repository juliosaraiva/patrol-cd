// Package clusters gives Patrol CD components access to destination clusters.
//
// It covers five concerns:
//
//   - Connection configuration. [RestConfig] and [RawRestConfig] turn a [v1alpha1.Cluster]
//     and the data of the Secret named by spec.config.credentialsRef into a *rest.Config,
//     with the same authentication precedence as Argo CD v3.5.5's Cluster.RawRestConfig:
//     the in-cluster server first, then awsAuthConfig, execProviderConfig, and finally
//     bearer-token and basic authentication, each with the TLS settings. EKS, GKE and AKS
//     tokens are produced in-process ([AWSToken], [GCPTokenSource], [AzureTokenSource]) so
//     no helper binary is needed; the exec form (`patrolcd k8s-auth ...`, package
//     [github.com/juliosaraiva/patrol-cd/internal/clusters/k8sauth]) remains available.
//     [ApplyTuning] bakes the client tuning knobs (QPS, burst, timeouts, retries) into a
//     custom transport as upstream SetK8SConfigDefaults does.
//   - The cluster registry. [Registry] is an informer-backed, read-only view of the Cluster
//     objects and their credential Secrets in the control-plane namespace, plus the
//     synthetic in-cluster cluster. It resolves Application destinations with upstream
//     error messages and notifies subscribers of effective changes.
//   - Connection state. [Probe] checks connectivity and returns the normalized server
//     version; [StatusWriter] persists Cluster status with retries; [ComputeClusterInfo]
//     maps live-state cache information to the upstream connection-state semantics.
//   - Impersonation. [ImpersonatingConfig] applies the service account that internal/project
//     selects from an AppProject's destinationServiceAccounts.
//   - Bootstrap. [EnsureManagerServiceAccount], [RotateManagerToken] and
//     [UninstallManager] install and maintain the patrol-manager service account that
//     `patrol cluster add` registers, mirroring upstream util/clusterauth.
//
// Credentials are never logged. Errors that may contain configuration never include Secret
// data.
package clusters
