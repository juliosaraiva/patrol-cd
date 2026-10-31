/**
 * GENERATED FILE - DO NOT EDIT. Run `pnpm -C ui gen:api`.
 * Source: api/openapi/openapi.json (normalized by ui/scripts/gen-api-types.mjs)
 */

export interface paths {
    "/api/badge": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * Status badge (SVG)
         * @description Health and sync status of an Application (`name`, `appNamespace`), or the aggregate of every Application of the `project`s, as an SVG image. `revision=true` adds the last synced revision (7 characters unless `keepFullRevision=true`), `showAppName=true` a title row, `width` overrides the width.
         */
        get: operations["getBadge"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/account": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /** List accounts */
        get: operations["listAccounts"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/account/can-i/{resource}/{action}/{subresource}": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /** RBAC check for the caller (subresource may contain slashes) */
        get: operations["canI"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/account/password": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        /** Change password */
        put: operations["updatePassword"];
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/account/{name}": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /** Get account */
        get: operations["getAccount"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/account/{name}/token": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        put?: never;
        /** Create API token */
        post: operations["createAccountToken"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/account/{name}/token/{id}": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        put?: never;
        post?: never;
        /** Delete API token */
        delete: operations["deleteAccountToken"];
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/applications": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * List applications
         * @description Applications of the enabled namespaces the caller may get, sorted by name. `limit` and `continue` paginate.
         */
        get: operations["listApplications"];
        put?: never;
        /**
         * Create an application
         * @description Idempotent when the existing application is identical; `upsert` updates a different one (needs update). `validate` (default true) renders every source.
         */
        post: operations["createApplication"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/applications/{name}": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * Get an application
         * @description `refresh=normal|hard` sets the refresh annotation and waits (up to 15 s) until the controller reconciled the application.
         */
        get: operations["getApplication"];
        /**
         * Update an application
         * @description Replaces spec, labels, annotations and finalizers; status is ignored. A `metadata.resourceVersion` makes the update conditional (409 when stale).
         */
        put: operations["updateApplication"];
        post?: never;
        /**
         * Delete an application
         * @description `cascade` (default true) adds the propagation finalizer of `propagationPolicy` (foreground default, background); `cascade=false` removes the propagation finalizers.
         */
        delete: operations["deleteApplication"];
        options?: never;
        head?: never;
        /** Patch an application (JSON or merge patch) */
        patch: operations["patchApplication"];
        trace?: never;
    };
    "/api/v1/applications/{name}/events": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * Kubernetes events of an application or of one of its resources
         * @description Without `resourceName`/`resourceUID`: the application's events. Otherwise the resource must be in the tree with that UID, name and namespace; its events are read from the destination cluster.
         */
        get: operations["listApplicationEvents"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/applications/{name}/links": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /** Deep links of an application (application.links) */
        get: operations["listApplicationLinks"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/applications/{name}/logs": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * Pod logs of an application (Server-Sent Events)
         * @description LogEntry events of the tree's pods matching `kind`/`group`/`resourceName`/`namespace` (directly or through an owner; at most server.maxPodLogsToRender pods), merged by timestamp, then an entry with `last: true`. Without a matching pod the stream ends without events.
         */
        get: operations["getApplicationLogs"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/applications/{name}/managed-resources": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * Managed resources with diff data (Secret data masked)
         * @description Hooks are excluded; non-empty `group`, `kind`, `namespace`, `resourceName` filters match exactly.
         */
        get: operations["getManagedResources"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/applications/{name}/manifests": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /** Render the target manifests (Secret data masked) */
        get: operations["getApplicationManifests"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/applications/{name}/manifests-with-files": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        put?: never;
        /** Render a local source directory (gzipped tar body) */
        post: operations["getApplicationManifestsWithFiles"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/applications/{name}/operation": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        put?: never;
        post?: never;
        /** Terminate the running operation (sets spec.cancel) */
        delete: operations["terminateApplicationOperation"];
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/applications/{name}/operations": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /** List the operations of an application (newest first) */
        get: operations["listApplicationOperations"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/applications/{name}/operations/{op}": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /** Get an operation of an application */
        get: operations["getApplicationOperation"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/applications/{name}/pods/{pod}/logs": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * Logs of one pod of an application (Server-Sent Events)
         * @description LogEntry events of the pod, which must be in the application's resource tree (otherwise the stream ends without events), then an entry with `last: true`.
         */
        get: operations["getPodLogs"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/applications/{name}/resource": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /** Live manifest of a resource of the application tree (Secret data masked) */
        get: operations["getResource"];
        put?: never;
        /**
         * Patch a live resource of the application tree
         * @description Body `{patch, patchType: json|merge|strategic}`; returns the patched manifest (Secret data masked).
         */
        post: operations["patchResource"];
        /**
         * Delete a live resource of the application tree
         * @description `orphan` deletes with propagation Orphan, `force` with Background and grace period 0, otherwise Foreground.
         */
        delete: operations["deleteResource"];
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/applications/{name}/resource-tree": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * Resource tree of an application
         * @description The tree computed by the application controller (managed resources and their descendants, orphaned resources, hosts). A missing tree triggers a normal refresh first.
         */
        get: operations["getResourceTree"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/applications/{name}/resource/actions": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * Actions of a resource (Lua discovery)
         * @description Group `cd.patrol.example`, kind `Application` and `resourceName` = the application's name target the application itself.
         */
        get: operations["listResourceActions"];
        put?: never;
        /**
         * Run a resource action
         * @description Body `{action, resourceActionParameters}`; parameters must be declared by the action's discovery; disabled actions are refused.
         */
        post: operations["runResourceAction"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/applications/{name}/resource/links": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /** Deep links of a managed resource (resource.links) */
        get: operations["listApplicationResourceLinks"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/applications/{name}/revisions/{revision}/chartdetails": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /** Helm chart metadata of a revision */
        get: operations["getApplicationRevisionChartDetails"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/applications/{name}/revisions/{revision}/metadata": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /** Git commit metadata of a revision */
        get: operations["getApplicationRevisionMetadata"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/applications/{name}/revisions/{revision}/ocimetadata": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /** OCI metadata of a revision */
        get: operations["getApplicationRevisionOCIMetadata"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/applications/{name}/rollback": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        put?: never;
        /** Roll back an application to a history entry (creates an Operation) */
        post: operations["rollbackApplication"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/applications/{name}/server-side-diff": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        put?: never;
        /**
         * Server-side (dry-run apply) diff
         * @description Index-aligned `liveResources` and `targetManifests` (at most 5000). Runs on the controller replica serving the application, with its credentials (not impersonated).
         */
        post: operations["serverSideDiff"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/applications/{name}/spec": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        /** Update an application spec */
        put: operations["updateApplicationSpec"];
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/applications/{name}/sync": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        put?: never;
        /**
         * Sync an application (creates an Operation)
         * @description Refused while another operation is in progress (409), inside a sync window that blocks manual syncs (403) and for applications being deleted.
         */
        post: operations["syncApplication"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/applications/{name}/syncwindows": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /** Sync window state of an application */
        get: operations["getApplicationSyncWindows"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/applicationsets": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * List ApplicationSets
         * @description ApplicationSets in enabled namespaces the caller may get, sorted by name; `projects`, `selector` and `appsetNamespace` filter, `limit`/`continue` paginate.
         */
        get: operations["listApplicationSets"];
        put?: never;
        /**
         * Create ApplicationSet
         * @description Creates the ApplicationSet; an identical existing one is returned unchanged, a different one needs `upsert=true`. `dryRun=true` renders the generated Applications into `status.resources` without persisting.
         */
        post: operations["createApplicationSet"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/applicationsets/generate": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        put?: never;
        /**
         * Dry-run render of generated Applications
         * @description Renders the Applications the ApplicationSet would generate (generators, templates, templatePatch) without validating destinations and without writing anything.
         */
        post: operations["generateApplicationSet"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/applicationsets/{name}": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /** Get ApplicationSet */
        get: operations["getApplicationSet"];
        put?: never;
        post?: never;
        /**
         * Delete ApplicationSet
         * @description `cascade=false` orphans the generated Applications; `propagationPolicy` is `foreground` or `background` (default).
         */
        delete: operations["deleteApplicationSet"];
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/applicationsets/{name}/events": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /** ApplicationSet events */
        get: operations["listApplicationSetEvents"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/applicationsets/{name}/resource-tree": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * ApplicationSet resource tree
         * @description One node per generated Application of `status.resources`, with health and sync status, parented by the ApplicationSet.
         */
        get: operations["getApplicationSetResourceTree"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/auth/callback": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /** OIDC callback (IdP redirect target) */
        get: operations["ssoCallback"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/auth/login": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /** Start the OIDC login flow (browser navigation) */
        get: operations["startSSOLogin"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/certificates": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /** List TLS certificates and SSH known hosts */
        get: operations["listCertificates"];
        put?: never;
        /**
         * Add TLS certificates and SSH known hosts
         * @description Validates every entry first (nothing is saved when one fails). Existing entries with different data need `upsert`; identical entries are skipped and not reported.
         */
        post: operations["createCertificates"];
        /**
         * Remove TLS certificates and SSH known hosts
         * @description Removes every entry matching the query and returns them; an empty query removes every entry (upstream API semantics).
         */
        delete: operations["deleteCertificates"];
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/clusters": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * List clusters
         * @description Lists the registered clusters the caller may get, including the implicit in-cluster cluster when cluster.inClusterEnabled is not "false". The server, name and id filters each keep only the first match. Credentials are never returned.
         */
        get: operations["listClusters"];
        put?: never;
        /**
         * Register a cluster
         * @description Validates the configuration, checks connectivity with GET /version, then stores the Cluster object and its credentials Secret. An identical existing cluster is returned unchanged; a different one is an error unless upsert is set (then the update rules apply). The implicit in-cluster cluster counts as existing: registering its server follows the update rules even without upsert. awsAuthConfig and execProviderConfig need the permission on the unscoped server object and are not probed by the API server.
         */
        post: operations["createCluster"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/clusters/{server}": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /** Get a cluster */
        get: operations["getCluster"];
        /**
         * Update a cluster
         * @description With updatedFields only the listed paths change (applied to the latest stored object, so concurrent changes of other fields are kept); without it the cluster (including its credentials) is replaced. The server cannot change. Connectivity is checked before saving.
         */
        put: operations["updateCluster"];
        post?: never;
        /** Delete a cluster (by name: every cluster with the name) */
        delete: operations["deleteCluster"];
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/clusters/{server}/invalidate-cache": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        put?: never;
        /** Request a rebuild of the controller's cache of the cluster */
        post: operations["invalidateClusterCache"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/clusters/{server}/rotate-auth": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        put?: never;
        /**
         * Rotate the cluster's service-account token
         * @description For clusters authenticated with a legacy service-account bearer token: creates a new token Secret in the destination cluster, verifies and stores the new token, then deletes the old token Secret.
         */
        post: operations["rotateClusterAuth"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/gpgkeys": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /** List GnuPG public keys */
        get: operations["listGPGKeys"];
        put?: never;
        /**
         * Add GnuPG public keys
         * @description `keyData` holds one or more ASCII-armored public keys; keys already configured are reported in `skipped`. Private keys are rejected.
         */
        post: operations["createGPGKeys"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/gpgkeys/{keyID}": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * Get a GnuPG public key
         * @description `keyID` is the 16-hex-digit key ID or the 40-hex-digit fingerprint.
         */
        get: operations["getGPGKey"];
        put?: never;
        post?: never;
        /** Remove a GnuPG public key */
        delete: operations["deleteGPGKey"];
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/notifications/services": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * Configured notification services
         * @description Names of the services of patrol-notifications-cm (never their configuration); empty when the ConfigMap does not exist.
         */
        get: operations["listNotificationServices"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/notifications/templates": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * Configured notification templates
         * @description Names of the templates of patrol-notifications-cm, including the built-in catalog; empty when the ConfigMap does not exist.
         */
        get: operations["listNotificationTemplates"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/notifications/triggers": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * Configured notification triggers
         * @description Names (and condition descriptions) of the triggers of patrol-notifications-cm, including the built-in catalog; empty when the ConfigMap does not exist.
         */
        get: operations["listNotificationTriggers"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/projects": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /** List projects */
        get: operations["listProjects"];
        put?: never;
        /**
         * Create a project
         * @description Validates the project (internal/project), requires clusters, update on each destination server and repositories, update on each source repository, and creates it. An identical existing project is returned; a different one is an error unless upsert is set.
         */
        post: operations["createProject"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/projects/{name}": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /** Get a project */
        get: operations["getProject"];
        /**
         * Update a project
         * @description Replaces the spec, labels and annotations. Changes that would make existing Applications of the project invalid are refused.
         */
        put: operations["updateProject"];
        post?: never;
        /**
         * Delete a project
         * @description The default project cannot be deleted, nor a project referenced by Applications or ApplicationSets of the control-plane namespace.
         */
        delete: operations["deleteProject"];
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/projects/{name}/detailed": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /** Get the effective project with global projects and scoped repositories and clusters */
        get: operations["getDetailedProject"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/projects/{name}/events": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /** Kubernetes Events of a project */
        get: operations["listProjectEvents"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/projects/{name}/globalprojects": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /** Global projects a project inherits from */
        get: operations["getGlobalProjects"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/projects/{name}/links": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /** Project deep links (project.links) */
        get: operations["listProjectLinks"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/projects/{name}/syncwindows": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /** Sync windows of a project active now */
        get: operations["getProjectSyncWindows"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/projects/{project}/roles/{role}/token": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        put?: never;
        /**
         * Create a project role token
         * @description Returns the token once; the project records its ID and times in status.jwtTokensByRole.
         */
        post: operations["createProjectToken"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/projects/{project}/roles/{role}/token/{iat}": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        put?: never;
        post?: never;
        /**
         * Delete (revoke) a project role token
         * @description Locates the token by ?id= when given, else by issued-at time. Unknown roles and tokens are not an error.
         */
        delete: operations["deleteProjectToken"];
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/repocreds": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /** List credential templates */
        get: operations["listRepositoryCredentials"];
        put?: never;
        /**
         * Create a credential template
         * @description Creates the RepositoryCredential object and the Secret holding `credentials`. An existing template with the same URL prefix is returned unchanged when identical, updated with `upsert`, rejected otherwise.
         */
        post: operations["createRepositoryCredential"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/repocreds/{url}": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        /**
         * Update a credential template
         * @description The template is addressed by its exact URL prefix (URL-encoded).
         */
        put: operations["updateRepositoryCredential"];
        post?: never;
        /**
         * Delete a credential template
         * @description Deletes the template with exactly this URL prefix and the credentials Secret managed for it.
         */
        delete: operations["deleteRepositoryCredential"];
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/repositories": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * List repository objects
         * @description Repositories the caller may get, sorted by project and URL, with their connection state; a missing or outdated state (older than the connection state TTL) is checked and recorded, `forceRefresh` checks every listed repository.
         */
        get: operations["listRepositories"];
        put?: never;
        /**
         * Create a repository
         * @description Creates the Repository object and the Secret holding `credentials`. Unscoped read repositories and every write repository are tested on the repo-server first; project-scoped read repositories are created even when unreachable and their state is recorded. An existing repository with the same URL and project is returned unchanged when identical, updated with `upsert`, rejected otherwise. `credsOnly` creates a credential template instead.
         */
        post: operations["createRepository"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/repositories/{repo}": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * Get a repository
         * @description The URL is URL-encoded in the path. `appProject` selects among repositories registered for the same URL in several projects; `forceRefresh` checks the connection.
         */
        get: operations["getRepository"];
        /**
         * Update a repository
         * @description Replaces the spec (`metadata.resourceVersion` for optimistic concurrency, 409 when stale). `credentials` replaces the stored credentials; without it they are kept. `?appProject=` names the current project when the repository moves to another one. The connection is checked and recorded.
         */
        put: operations["updateRepository"];
        post?: never;
        /**
         * Delete a repository
         * @description Deletes the Repository object and the credentials Secret managed for it; Secrets not managed by the API are kept.
         */
        delete: operations["deleteRepository"];
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/repositories/{repo}/appdetails": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        put?: never;
        /** Tool-specific details of an application source */
        post: operations["getRepositoryAppDetails"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/repositories/{repo}/apps": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /** Discover application directories */
        get: operations["listRepositoryApps"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/repositories/{repo}/helmcharts": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /** List the charts of a Helm repository */
        get: operations["listRepositoryHelmCharts"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/repositories/{repo}/oci-tags": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /** List tags of an OCI repository */
        get: operations["listRepositoryOCITags"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/repositories/{repo}/refs": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * List branches and tags of a git repository
         * @description Credentials are resolved server-side from the registered repository (scoped to `appProject` when given), its credential template or SCM provider.
         */
        get: operations["listRepositoryRefs"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/repositories/{repo}/validate": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        put?: never;
        /**
         * Test a repository connection without saving
         * @description Tests the settings and credentials of the body on the repo-server; nothing is stored. Read repositories without credentials use a matching credential template. A failed test answers 400 with the connection message.
         */
        post: operations["validateRepositoryAccess"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/scmproviders": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /** List SCM providers */
        get: operations["listSCMProviders"];
        put?: never;
        /**
         * Create an SCM provider
         * @description Secrets are referenced by name only; the API never accepts secret values for providers. Granting create or update lets the caller send the referenced Secrets' credentials to the provider URLs it chooses.
         */
        post: operations["createSCMProvider"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/scmproviders/{name}": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /** Get an SCM provider */
        get: operations["getSCMProvider"];
        /**
         * Update an SCM provider
         * @description Replaces spec, labels and annotations (`metadata.resourceVersion` for optimistic concurrency, 409 when stale).
         */
        put: operations["updateSCMProvider"];
        post?: never;
        /** Delete an SCM provider */
        delete: operations["deleteSCMProvider"];
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/session": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        put?: never;
        /**
         * Local login
         * @description Verifies a local account password (with per-account lockout and a global concurrency limit) and returns a session token; `?cookie=true` also sets the patrol.token cookie.
         */
        post: operations["createSession"];
        /**
         * Logout (revokes the session)
         * @description Revokes the caller's login or SSO session until it expires, clears the session cookie and returns the identity provider logout URL for SSO sessions when configured.
         */
        delete: operations["deleteSession"];
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/session/oidc-exchange": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        put?: never;
        /** Exchange an ID token for a Patrol session (CLI SSO) */
        post: operations["exchangeOIDCToken"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/session/userinfo": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /** Current user */
        get: operations["getUserInfo"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/settings": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /** UI/CLI settings (public subset when unauthenticated) */
        get: operations["getSettings"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/settings/plugins": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /** Config management plugins */
        get: operations["getPluginSettings"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/stream/applications": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * Watch applications (Server-Sent Events)
         * @description Events `{type: ADDED|MODIFIED|DELETED, object}`. An application the caller can no longer see is reported as DELETED with its last visible state.
         */
        get: operations["watchApplications"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/stream/applications/{name}/resource-tree": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * Watch the resource tree (Server-Sent Events)
         * @description Events `{type, object: ApplicationTree}`: ADDED with the current tree, MODIFIED when it changes, DELETED when the application is deleted. Losing access ends the stream with a 403 error event.
         */
        get: operations["watchResourceTree"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/stream/applicationsets": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * Watch ApplicationSets (Server-Sent Events)
         * @description Events `{type: ADDED|MODIFIED|DELETED, object}`. Without `resourceVersion`, the stream starts now and, with `name`, sends the current state first (ADDED). A `resourceVersion` (e.g. of a previous list) or `Last-Event-ID` starts the watch there, so no change is missed; an expired version ends the stream with a 410 error event.
         */
        get: operations["watchApplicationSets"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/version": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * Build and tool versions
         * @description Callers that are not logged in only receive `version`, unless anonymous access is enabled.
         */
        get: operations["getVersion"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/write-repocreds": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /** List write credential templates */
        get: operations["listWriteRepositoryCredentials"];
        put?: never;
        /**
         * Create a write credential template
         * @description Creates the RepositoryCredential object and the Secret holding `credentials`. An existing template with the same URL prefix is returned unchanged when identical, updated with `upsert`, rejected otherwise.
         */
        post: operations["createWriteRepositoryCredential"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/write-repocreds/{url}": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        /**
         * Update a write credential template
         * @description The template is addressed by its exact URL prefix (URL-encoded).
         */
        put: operations["updateWriteRepositoryCredential"];
        post?: never;
        /**
         * Delete a write credential template
         * @description Deletes the template with exactly this URL prefix and the credentials Secret managed for it.
         */
        delete: operations["deleteWriteRepositoryCredential"];
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/write-repositories": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * List write repository objects
         * @description Repositories the caller may get, sorted by project and URL, with their connection state; a missing or outdated state (older than the connection state TTL) is checked and recorded, `forceRefresh` checks every listed repository. Answers 501 unless the source hydrator is enabled.
         */
        get: operations["listWriteRepositories"];
        put?: never;
        /**
         * Create a write repository
         * @description Creates the Repository object and the Secret holding `credentials`. Unscoped read repositories and every write repository are tested on the repo-server first; project-scoped read repositories are created even when unreachable and their state is recorded. An existing repository with the same URL and project is returned unchanged when identical, updated with `upsert`, rejected otherwise. `credsOnly` creates a credential template instead. Answers 501 unless the source hydrator is enabled.
         */
        post: operations["createWriteRepository"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/write-repositories/{repo}": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * Get a write repository
         * @description The URL is URL-encoded in the path. `appProject` selects among repositories registered for the same URL in several projects; `forceRefresh` checks the connection. Answers 501 unless the source hydrator is enabled.
         */
        get: operations["getWriteRepository"];
        /**
         * Update a write repository
         * @description Replaces the spec (`metadata.resourceVersion` for optimistic concurrency, 409 when stale). `credentials` replaces the stored credentials; without it they are kept. `?appProject=` names the current project when the repository moves to another one. The connection is checked and recorded. Answers 501 unless the source hydrator is enabled.
         */
        put: operations["updateWriteRepository"];
        post?: never;
        /**
         * Delete a write repository
         * @description Deletes the Repository object and the credentials Secret managed for it; Secrets not managed by the API are kept. Answers 501 unless the source hydrator is enabled.
         */
        delete: operations["deleteWriteRepository"];
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/write-repositories/{repo}/validate": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        put?: never;
        /**
         * Test a write repository connection without saving
         * @description Tests the settings and credentials of the body on the repo-server; nothing is stored. Read repositories without credentials use a matching credential template. A failed test answers 400 with the connection message. Answers 501 unless the source hydrator is enabled.
         */
        post: operations["validateWriteRepositoryAccess"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/version": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /** Build and tool versions (alias) */
        get: operations["getVersionAlias"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/webhook": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        put?: never;
        /**
         * Receive an SCM or registry webhook (provider detected from the headers)
         * @description Detects the provider from the delivery's headers (x-patrol-webhook-receivers, in order), verifies it and refreshes the matching Applications and ApplicationSets asynchronously. SCM providers are configured with the URL <server>/api/webhook or <server>/api/webhook/{provider}.
         */
        post: operations["receiveWebhook"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/webhook/{provider}": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        put?: never;
        /**
         * Receive a webhook for a named provider
         * @description Same as POST /api/webhook with the receiver named in the path; the delivery's headers must identify that provider.
         */
        post: operations["receiveProviderWebhook"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/extensions/{extension}/{path}": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * Proxy extension call (GET)
         * @description Forwards the request (path after the extension name, query, allow-listed headers, body) to the extension backend selected by the Application's destination cluster. Requires the headers `Patrol-Application-Name: <namespace>:<name>` and `Patrol-Project-Name: <project>`. Disabled unless server.enable.proxy.extension is true.
         */
        get: operations["invokeExtensionGet"];
        /**
         * Proxy extension call (PUT)
         * @description Forwards the request (path after the extension name, query, allow-listed headers, body) to the extension backend selected by the Application's destination cluster. Requires the headers `Patrol-Application-Name: <namespace>:<name>` and `Patrol-Project-Name: <project>`. Disabled unless server.enable.proxy.extension is true.
         */
        put: operations["invokeExtensionPut"];
        /**
         * Proxy extension call (POST)
         * @description Forwards the request (path after the extension name, query, allow-listed headers, body) to the extension backend selected by the Application's destination cluster. Requires the headers `Patrol-Application-Name: <namespace>:<name>` and `Patrol-Project-Name: <project>`. Disabled unless server.enable.proxy.extension is true.
         */
        post: operations["invokeExtensionPost"];
        /**
         * Proxy extension call (DELETE)
         * @description Forwards the request (path after the extension name, query, allow-listed headers, body) to the extension backend selected by the Application's destination cluster. Requires the headers `Patrol-Application-Name: <namespace>:<name>` and `Patrol-Project-Name: <project>`. Disabled unless server.enable.proxy.extension is true.
         */
        delete: operations["invokeExtensionDelete"];
        options?: never;
        head?: never;
        /**
         * Proxy extension call (PATCH)
         * @description Forwards the request (path after the extension name, query, allow-listed headers, body) to the extension backend selected by the Application's destination cluster. Requires the headers `Patrol-Application-Name: <namespace>:<name>` and `Patrol-Project-Name: <project>`. Disabled unless server.enable.proxy.extension is true.
         */
        patch: operations["invokeExtensionPatch"];
        trace?: never;
    };
    "/terminal": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * Web terminal (WebSocket)
         * @description WebSocket of TerminalMessage JSON text frames (stdin and resize from the client, stdout from the server) to a running container of a pod of the application's tree. Same-origin handshakes only.
         */
        get: operations["openTerminal"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
}
export type webhooks = Record<string, never>;
export interface components {
    schemas: {
        /** @description Account is a local account. */
        Account: {
            capabilities?: string[];
            enabled: boolean;
            name: string;
            tokens?: components["schemas"]["AccountToken"][];
        };
        /** @description AccountList is the response of GET /api/v1/account. */
        AccountList: {
            items: components["schemas"]["Account"][];
        };
        /** @description AccountToken is the metadata of an account API token. */
        AccountToken: {
            /** @description ExpiresAt is a Unix timestamp (seconds); 0 means the token does not expire. */
            expiresAt?: number;
            id: string;
            /** @description IssuedAt is a Unix timestamp (seconds). */
            issuedAt?: number;
        };
        /**
         * @description AppProject groups Applications and restricts what they may deploy (source repositories, resource
         *     kinds), where (destinations, sync windows), and who may act on them (roles, tokens).
         */
        AppProject: {
            /**
             * @description APIVersion defines the versioned schema of this representation of an object.
             *     Servers should convert recognized schemas to the latest internal value, and
             *     may reject unrecognized values.
             *     More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources
             * @default cd.patrol.example/v1alpha1
             */
            apiVersion?: string;
            /**
             * @description Kind is a string value representing the REST resource this object represents.
             *     Servers may infer this from the endpoint the client submits requests to.
             *     Cannot be updated.
             *     In CamelCase.
             *     More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds
             */
            kind?: string;
            metadata?: components["schemas"]["ObjectMeta"];
            /** @description Spec is the project policy. */
            spec: {
                /** @description ClusterResourceBlacklist lists cluster-scoped kinds Applications may not manage. */
                clusterResourceBlacklist?: components["schemas"]["AppProjectSpecClusterResourceWhitelistItem"][];
                /** @description ClusterResourceWhitelist lists the cluster-scoped kinds Applications may manage. */
                clusterResourceWhitelist?: components["schemas"]["AppProjectSpecClusterResourceWhitelistItem"][];
                /** @description Description of the project. */
                description?: string;
                /** @description DestinationServiceAccounts select the service account impersonated when syncing to a destination. */
                destinationServiceAccounts?: {
                    /** @description DefaultServiceAccount is the service account to impersonate ("<namespace>:<name>" or "<name>"). */
                    defaultServiceAccount: string;
                    /** @description Namespace is the destination namespace (glob). */
                    namespace?: string;
                    /** @description Server is the destination server URL (glob). */
                    server: string;
                }[];
                /** @description Destinations are the clusters and namespaces Applications may deploy to. */
                destinations?: components["schemas"]["ApplicationSpecDestination"][];
                /** @description NamespaceResourceBlacklist lists namespaced kinds Applications may not manage. */
                namespaceResourceBlacklist?: {
                    group: string;
                    kind: string;
                }[];
                /** @description NamespaceResourceWhitelist lists the namespaced kinds Applications may manage (all if empty). */
                namespaceResourceWhitelist?: {
                    group: string;
                    kind: string;
                }[];
                /** @description OrphanedResources configures orphaned-resource monitoring. */
                orphanedResources?: {
                    /** @description Ignore lists resources excluded from monitoring. */
                    ignore?: {
                        group?: string;
                        kind?: string;
                        name?: string;
                    }[];
                    /** @description Warn adds the OrphanedResourceWarning condition to Applications with orphaned resources. */
                    warn?: boolean;
                };
                /**
                 * @description PermitOnlyProjectScopedClusters restricts destinations to Clusters with spec.project set to this
                 *     project.
                 */
                permitOnlyProjectScopedClusters?: boolean;
                /** @description Roles are project-scoped RBAC roles. */
                roles?: {
                    /** @description Description of the role. */
                    description?: string;
                    /** @description Groups are OIDC groups bound to the role. */
                    groups?: string[];
                    /**
                     * @description JWTTokens are tokens issued for the role.
                     *
                     *     Deprecated: tokens are recorded in status.jwtTokensByRole; this field is read for migration.
                     */
                    jwtTokens?: components["schemas"]["AppProjectSpecRolesItemJwtTokensItem"][];
                    /** @description Name of the role. */
                    name: string;
                    /** @description Policies are Casbin-style policy lines ("p, proj:<project>:<role>, <resource>, <action>, <object>, <effect>"). */
                    policies?: string[];
                }[];
                /**
                 * @description SignatureKeys are GPG key IDs commits must be signed with.
                 *
                 *     Deprecated: use SourceIntegrity. Kept for migration; treated as a git GPG "head" policy for "*".
                 */
                signatureKeys?: {
                    /** @description KeyID in hexadecimal notation. */
                    keyID: string;
                }[];
                /** @description SourceIntegrity are verification requirements sources must meet before use. */
                sourceIntegrity?: {
                    /** @description Git holds the git verification policies. */
                    git: {
                        policies: {
                            /** @description GPG verifies commit/tag signatures. */
                            gpg: {
                                /** @description Keys are the trusted key IDs; they must be present in the GPG keyring. */
                                keys: string[];
                                /**
                                 * @description SourceIntegrityGitPolicyGPGMode is the GPG verification mode.
                                 * @enum {string}
                                 */
                                mode: "none" | "head" | "strict";
                            };
                            /** @description Repos restrict the repositories the policy applies to. */
                            repos: {
                                /** @description URL glob. */
                                url: string;
                            }[];
                        }[];
                    };
                };
                /** @description SourceNamespaces are the namespaces (globs) Applications of this project may be created in. */
                sourceNamespaces?: string[];
                /** @description SourceRepos are the repository URLs (globs, "!" negation) Applications may use. */
                sourceRepos?: string[];
                /** @description SyncWindows restrict when syncs may run. */
                syncWindows?: components["schemas"]["AppProjectSpecSyncWindowsItem"][];
            };
            /** @description Status is the observed state of the project. */
            status?: {
                /** @description Conditions are the current project conditions. */
                conditions?: {
                    /**
                     * Format: date-time
                     * @description lastTransitionTime is the last time the condition transitioned from one status to another.
                     *     This should be when the underlying condition changed.  If that is not known, then using the time when the API field changed is acceptable.
                     */
                    lastTransitionTime: string;
                    /**
                     * @description message is a human readable message indicating details about the transition.
                     *     This may be an empty string.
                     */
                    message: string;
                    /**
                     * Format: int64
                     * @description observedGeneration represents the .metadata.generation that the condition was set based upon.
                     *     For instance, if .metadata.generation is currently 12, but the .status.conditions[x].observedGeneration is 9, the condition is out of date
                     *     with respect to the current state of the instance.
                     */
                    observedGeneration?: number;
                    /**
                     * @description reason contains a programmatic identifier indicating the reason for the condition's last transition.
                     *     Producers of specific condition types may define expected values and meanings for this field,
                     *     and whether the values are considered a guaranteed API.
                     *     The value should be a CamelCase string.
                     *     This field may not be empty.
                     */
                    reason: string;
                    /**
                     * @description status of the condition, one of True, False, Unknown.
                     * @enum {string}
                     */
                    status: "True" | "False" | "Unknown";
                    /** @description type of condition in CamelCase or in foo.example.com/CamelCase. */
                    type: string;
                }[];
                /** @description JWTTokensByRole lists the tokens issued per role. */
                jwtTokensByRole?: {
                    [key: string]: {
                        items?: components["schemas"]["AppProjectSpecRolesItemJwtTokensItem"][];
                    };
                };
                /**
                 * Format: int64
                 * @description ObservedGeneration is the metadata.generation the status reflects.
                 */
                observedGeneration?: number;
            };
        };
        /** @description AppProject list. */
        AppProjectList: {
            apiVersion?: string;
            items: components["schemas"]["StoredAppProject"][];
            kind?: string;
            metadata?: components["schemas"]["ListMeta"];
        };
        /**
         * @description Application deploys the manifests rendered from one or more sources to a destination cluster and
         *     namespace, under the policy of an AppProject.
         *
         *     Unlike Argo CD there is no top-level `operation` field: sync and rollback requests are Operation
         *     objects owned by the Application, and status.operationState summarizes the most recent one.
         */
        Application: {
            /**
             * @description APIVersion defines the versioned schema of this representation of an object.
             *     Servers should convert recognized schemas to the latest internal value, and
             *     may reject unrecognized values.
             *     More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources
             * @default cd.patrol.example/v1alpha1
             */
            apiVersion?: string;
            /**
             * @description Kind is a string value representing the REST resource this object represents.
             *     Servers may infer this from the endpoint the client submits requests to.
             *     Cannot be updated.
             *     In CamelCase.
             *     More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds
             */
            kind?: string;
            metadata?: components["schemas"]["ObjectMeta"];
            spec: components["schemas"]["ApplicationSpec"];
            /** @description Status is the observed state of the Application, written by the controller. */
            status?: {
                /** @description Conditions are the current Application conditions (see conditions.go for types and reasons). */
                conditions?: {
                    /**
                     * Format: date-time
                     * @description lastTransitionTime is the last time the condition transitioned from one status to another.
                     *     This should be when the underlying condition changed.  If that is not known, then using the time when the API field changed is acceptable.
                     */
                    lastTransitionTime: string;
                    /**
                     * @description message is a human readable message indicating details about the transition.
                     *     This may be an empty string.
                     */
                    message: string;
                    /**
                     * Format: int64
                     * @description observedGeneration represents the .metadata.generation that the condition was set based upon.
                     *     For instance, if .metadata.generation is currently 12, but the .status.conditions[x].observedGeneration is 9, the condition is out of date
                     *     with respect to the current state of the instance.
                     */
                    observedGeneration?: number;
                    /**
                     * @description reason contains a programmatic identifier indicating the reason for the condition's last transition.
                     *     Producers of specific condition types may define expected values and meanings for this field,
                     *     and whether the values are considered a guaranteed API.
                     *     The value should be a CamelCase string.
                     *     This field may not be empty.
                     */
                    reason: string;
                    /**
                     * @description status of the condition, one of True, False, Unknown.
                     * @enum {string}
                     */
                    status: "True" | "False" | "Unknown";
                    /** @description type of condition in CamelCase or in foo.example.com/CamelCase. */
                    type: string;
                }[];
                /** @description ControllerNamespace is the namespace of the controller that reconciles the Application. */
                controllerNamespace?: string;
                /** @description Health is the aggregated health of the managed resources. */
                health?: {
                    /**
                     * Format: date-time
                     * @description LastTransitionTime is when Status last changed.
                     */
                    lastTransitionTime?: string;
                    /** @description Status is the aggregated health status code. */
                    status?: string;
                };
                /** @description History holds the most recent successful syncs, oldest first (bounded by revisionHistoryLimit). */
                history?: {
                    /**
                     * Format: date-time
                     * @description DeployStartedAt is when the sync started.
                     */
                    deployStartedAt?: string;
                    /**
                     * Format: date-time
                     * @description DeployedAt is when the sync completed.
                     */
                    deployedAt: string;
                    /**
                     * Format: int64
                     * @description ID is an auto-incrementing identifier used by rollback.
                     */
                    id: number;
                    initiatedBy?: components["schemas"]["ApplicationStatusHistoryItemInitiatedBy"];
                    /**
                     * @description OperationName is the name of the Operation that performed the sync (it may have been deleted
                     *     by retention).
                     */
                    operationName?: string;
                    /** @description Revision is the revision synced for a single-source Application. */
                    revision?: string;
                    /** @description Revisions are the revisions synced for each source of a multi-source Application. */
                    revisions?: string[];
                    /** @description Source is the source used by the sync of a single-source Application. */
                    source?: components["schemas"]["ApplicationSpecSource"];
                    /** @description Sources are the sources used by the sync of a multi-source Application. */
                    sources?: components["schemas"]["ApplicationSpecSource"][];
                }[];
                /**
                 * Format: int64
                 * @description ObservedGeneration is the metadata.generation the status reflects.
                 */
                observedGeneration?: number;
                /**
                 * @description OperationState summarizes the most recent Operation of the Application. Per-resource results
                 *     live in the Operation's status.
                 */
                operationState?: {
                    /**
                     * Format: date-time
                     * @description FinishedAt is when the Operation reached a terminal phase.
                     */
                    finishedAt?: string;
                    /** @description InitiatedBy records who started the Operation. */
                    initiatedBy?: components["schemas"]["ApplicationStatusHistoryItemInitiatedBy"];
                    /** @description Message is the Operation's status message. */
                    message?: string;
                    /** @description OperationName is the name of the Operation in the Application's namespace. */
                    operationName: string;
                    /**
                     * @description Phase is the Operation's phase.
                     * @enum {string}
                     */
                    phase?: "Pending" | "Running" | "Terminating" | "Succeeded" | "Failed" | "Error" | "Cancelled" | "Rejected";
                    /**
                     * Format: int64
                     * @description RetryCount is the number of retries performed.
                     */
                    retryCount?: number;
                    /**
                     * Format: date-time
                     * @description StartedAt is when the Operation started running.
                     */
                    startedAt?: string;
                    /**
                     * @description SyncResult summarizes what was synced; used to decide whether automated sync already
                     *     attempted the current revision.
                     */
                    syncResult?: {
                        revision?: string;
                        revisions?: string[];
                        /**
                         * @description ApplicationSource is the location of an application's manifests or chart and the options used to
                         *     render them.
                         */
                        source?: components["schemas"]["ApplicationSpecSource"];
                        /** @description ApplicationSources is an ordered list of sources. */
                        sources?: components["schemas"]["ApplicationSpecSource"][];
                    };
                };
                /**
                 * Format: date-time
                 * @description ReconciledAt is when the Application was last compared against the latest revision.
                 */
                reconciledAt?: string;
                /** @description ResourceHealthSource is where per-resource health is stored: inline ("") or appTree. */
                resourceHealthSource?: string;
                /** @description Resources lists the resources managed by the Application (bounded by the controller). */
                resources?: {
                    /** @description Group is the API group of the resource ("" for the core group). */
                    group?: string;
                    health?: components["schemas"]["ApplicationStatusResourcesItemHealth"];
                    /** @description Hook is true if the resource is a sync hook. */
                    hook?: boolean;
                    /** @description Kind is the kind of the resource. */
                    kind?: string;
                    /** @description Name of the resource. */
                    name?: string;
                    /** @description Namespace of the resource ("" for cluster-scoped resources). */
                    namespace?: string;
                    /**
                     * @description RequiresDeletionConfirmation is true if deleting the resource requires explicit approval
                     *     (Delete=confirm or Prune=confirm sync options).
                     */
                    requiresDeletionConfirmation?: boolean;
                    /** @description RequiresPruning is true if the resource is live but no longer desired. */
                    requiresPruning?: boolean;
                    /** @description Status is the comparison result for this resource. */
                    status?: string;
                    /**
                     * Format: int64
                     * @description SyncWave is the sync wave of the resource.
                     */
                    syncWave?: number;
                    /** @description Version is the API version of the resource. */
                    version?: string;
                }[];
                /** @description SourceHydrator is the state of source hydration. */
                sourceHydrator?: {
                    /** @description CurrentOperation is the in-progress or most recent hydration. */
                    currentOperation?: {
                        /** @description DrySHA is the resolved dry revision. */
                        drySHA?: string;
                        /**
                         * Format: date-time
                         * @description FinishedAt is when hydration finished.
                         */
                        finishedAt?: string;
                        /** @description HydratedSHA is the commit containing the hydrated manifests. */
                        hydratedSHA?: string;
                        /** @description Message describes the hydration state. */
                        message?: string;
                        /**
                         * @description Phase of the hydration.
                         * @enum {string}
                         */
                        phase: "Hydrating" | "Failed" | "Hydrated";
                        /** @description SourceHydrator is the configuration used for the hydration. */
                        sourceHydrator?: components["schemas"]["ApplicationSpecSourceHydrator"];
                        /**
                         * Format: date-time
                         * @description StartedAt is when hydration started.
                         */
                        startedAt?: string;
                    };
                    /**
                     * @description LastComparedDryRevision is the dry revision of the latest comparison, updated even when
                     *     hydration was skipped because nothing changed.
                     */
                    lastComparedDryRevision?: string;
                    /** @description LastSuccessfulOperation is the most recent successful hydration. */
                    lastSuccessfulOperation?: {
                        drySHA?: string;
                        hydratedSHA?: string;
                        /**
                         * @description SourceHydrator configures source hydration: a dry source is rendered and committed to a branch,
                         *     and the Application syncs from that branch.
                         */
                        sourceHydrator?: components["schemas"]["ApplicationSpecSourceHydrator"];
                    };
                };
                /** @description SourceType is the detected rendering tool of a single-source Application. */
                sourceType?: string;
                /** @description SourceTypes are the detected rendering tools of each source of a multi-source Application. */
                sourceTypes?: string[];
                /** @description Summary lists external URLs and images used by the managed resources. */
                summary?: {
                    /** @description ExternalURLs of the managed resources (Ingress hosts, link annotations). */
                    externalURLs?: string[];
                    /** @description Images used by the managed resources. */
                    images?: string[];
                    /** @description IsAppOfApps is true if the Application manages other Applications. */
                    isAppOfApps?: boolean;
                };
                /** @description Sync is the result of the latest comparison. */
                sync?: {
                    /** @description ComparedTo is what the comparison was performed against. */
                    comparedTo?: {
                        /** @description Destination is the compared destination. */
                        destination?: components["schemas"]["ApplicationSpecDestination"];
                        /** @description IgnoreDifferences are the ignore rules used for the comparison. */
                        ignoreDifferences?: components["schemas"]["ApplicationSpecIgnoreDifferencesItem"][];
                        /** @description Source is the compared source of a single-source Application. */
                        source?: components["schemas"]["ApplicationSpecSource"];
                        /** @description Sources are the compared sources of a multi-source Application. */
                        sources?: components["schemas"]["ApplicationSpecSource"][];
                    };
                    /** @description Revision is the resolved revision of a single-source Application. */
                    revision?: string;
                    /** @description Revisions are the resolved revisions of each source of a multi-source Application. */
                    revisions?: string[];
                    /**
                     * @description ShortRevision is a display form of Revision/Revisions for printer columns (abbreviated commit
                     *     SHA or chart version; "+N" appended for additional sources). Not used for comparison.
                     */
                    shortRevision?: string;
                    /** @description Status is the comparison result. */
                    status?: string;
                };
            };
        };
        /** @description ApplicationChartDetails is Helm chart metadata. */
        ApplicationChartDetails: {
            description?: string;
            home?: string;
            maintainers?: string[];
        };
        /** @description Application list. */
        ApplicationList: {
            apiVersion?: string;
            items: components["schemas"]["StoredApplication"][];
            kind?: string;
            metadata?: components["schemas"]["ListMeta"];
        };
        /** @description ApplicationManifestResponse holds rendered manifests (JSON documents; Secret data masked). */
        ApplicationManifestResponse: {
            manifests: string[];
            /** @description Namespace and Server are the destination of the rendering. */
            namespace?: string;
            /** @description Revision is the rendered revision of a single-source Application. */
            revision?: string;
            /** @description Revisions are the rendered revisions per source of a multi-source Application. */
            revisions?: string[];
            server?: string;
            /** @description SourceType(s) are the detected rendering tools. */
            sourceType?: string;
            sourceTypes?: string[];
        };
        /** @description ApplicationOCIMetadata is the annotation metadata of an OCI artifact. */
        ApplicationOCIMetadata: {
            authors?: string;
            createdAt?: string;
            description?: string;
            docsUrl?: string;
            imageUrl?: string;
            sourceUrl?: string;
            version?: string;
        };
        /** @description ApplicationOperationResponse is the result of a sync, rollback or termination request: the Operation (created or cancelled) and the Application. */
        ApplicationOperationResponse: {
            application?: components["schemas"]["StoredApplication"];
            operation?: components["schemas"]["StoredOperation"];
        };
        /** @description ApplicationPatchRequest is PATCH /api/v1/applications/{name}. The Argo CD body fields name, appNamespace and project are accepted and must agree with the path and query. */
        ApplicationPatchRequest: {
            appNamespace?: string;
            name?: string;
            /** @description Patch is an RFC 6902 JSON patch (patchType json, the default) or an RFC 7386 merge patch (patchType merge) applied to the Application JSON. */
            patch: string;
            patchType?: string;
            project?: string;
        };
        /** @description ApplicationResourcePatchRequest patches a live resource of an Application's tree. PatchType is json, merge (default) or strategic; the media types application/json-patch+json, application/merge-patch+json and application/strategic-merge-patch+json are accepted too. */
        ApplicationResourcePatchRequest: {
            patch: string;
            patchType?: string;
        };
        /** @description ApplicationResourceResponse carries a live manifest (JSON; core Secret data masked). */
        ApplicationResourceResponse: {
            manifest: string;
        };
        /** @description ApplicationResponse is an empty acknowledgement. */
        ApplicationResponse: Record<string, never>;
        /** @description ApplicationRevisionMetadata is Git commit metadata. */
        ApplicationRevisionMetadata: {
            author?: string;
            /** Format: date-time */
            date?: string;
            message?: string;
            references?: components["schemas"]["ApplicationRevisionReference"][];
            signatureInfo?: string;
            sourceIntegrityResult?: components["schemas"]["ApplicationSourceIntegrityResult"];
            tags?: string[];
        };
        /** @description ApplicationRevisionReference is a commit referenced by a revision (hydrated commits). */
        ApplicationRevisionReference: {
            author?: string;
            body?: string;
            /** Format: date-time */
            date?: string;
            path?: string;
            repoURL?: string;
            sha?: string;
            subject?: string;
        };
        /** @description ApplicationRollbackRequest is POST /api/v1/applications/{name}/rollback. */
        ApplicationRollbackRequest: {
            appNamespace?: string;
            dryRun?: boolean;
            /** @description ID is the status.history entry to roll back to. */
            id: number;
            name?: string;
            project?: string;
            prune?: boolean;
        };
        /** @description ApplicationServerSideDiffRequest asks for a server-side (dry-run apply) diff of index-aligned live resources and target manifests: a live state that is empty or "null" is a resource to create, an empty or "null" target manifest a resource to prune. Every live resource must be a managed resource of the Application. */
        ApplicationServerSideDiffRequest: {
            liveResources?: components["schemas"]["ResourceDiff"][];
            targetManifests?: string[];
        };
        ApplicationSet: {
            /** @default cd.patrol.example/v1alpha1 */
            apiVersion?: string;
            kind?: string;
            metadata?: components["schemas"]["ObjectMeta"];
            spec: {
                applyNestedSelectors?: boolean;
                generators: {
                    clusterDecisionResource?: components["schemas"]["ApplicationSetSpecGeneratorsItemClusterDecisionResource"];
                    clusters?: components["schemas"]["ApplicationSetSpecGeneratorsItemClusters"];
                    git?: components["schemas"]["ApplicationSetSpecGeneratorsItemGit"];
                    list?: components["schemas"]["ApplicationSetSpecGeneratorsItemList"];
                    matrix?: {
                        generators: components["schemas"]["ApplicationSetSpecGeneratorsItemMatrixGeneratorsItem"][];
                        template?: components["schemas"]["ApplicationSetSpecGeneratorsItemListTemplate"];
                    };
                    merge?: {
                        generators: components["schemas"]["ApplicationSetSpecGeneratorsItemMatrixGeneratorsItem"][];
                        mergeKeys: string[];
                        template?: components["schemas"]["ApplicationSetSpecGeneratorsItemListTemplate"];
                    };
                    plugin?: components["schemas"]["ApplicationSetSpecGeneratorsItemMatrixGeneratorsItemPlugin"];
                    pullRequest?: components["schemas"]["ApplicationSetSpecGeneratorsItemPullRequest"];
                    scmProvider?: components["schemas"]["ApplicationSetSpecGeneratorsItemScmProvider"];
                    selector?: {
                        matchExpressions?: {
                            key: string;
                            operator: string;
                            values?: string[];
                        }[];
                        matchLabels?: {
                            [key: string]: string;
                        };
                    };
                }[];
                goTemplate?: boolean;
                goTemplateOptions?: string[];
                ignoreApplicationDifferences?: {
                    jqPathExpressions?: string[];
                    jsonPointers?: string[];
                    name?: string;
                }[];
                preservedFields?: {
                    annotations?: string[];
                    labels?: string[];
                };
                strategy?: {
                    /** @enum {string} */
                    deletionOrder?: "AllAtOnce" | "Reverse";
                    rollingSync?: {
                        steps?: {
                            matchExpressions?: {
                                key?: string;
                                /** @enum {string} */
                                operator?: "In" | "NotIn";
                                values?: string[];
                            }[];
                            maxUpdate?: number | string;
                        }[];
                    };
                    /** @enum {string} */
                    type?: "AllAtOnce" | "RollingSync";
                };
                syncPolicy?: {
                    /** @enum {string} */
                    applicationsSync?: "create-only" | "create-update" | "create-delete" | "sync";
                    preserveResourcesOnDeletion?: boolean;
                };
                template: components["schemas"]["ApplicationSetSpecGeneratorsItemListTemplate"];
                templatePatch?: string;
            };
            status?: {
                applicationStatus?: {
                    application: string;
                    /** Format: date-time */
                    lastTransitionTime?: string;
                    message?: string;
                    status: string;
                    step?: string;
                    targetRevisions?: string[];
                }[];
                conditions?: {
                    /** Format: date-time */
                    lastTransitionTime: string;
                    message: string;
                    /** Format: int64 */
                    observedGeneration?: number;
                    reason: string;
                    /** @enum {string} */
                    status: "True" | "False" | "Unknown";
                    type: string;
                }[];
                health?: {
                    message?: string;
                    status?: string;
                };
                /** Format: int64 */
                observedGeneration?: number;
                resources?: {
                    group?: string;
                    health?: {
                        message?: string;
                        status?: string;
                    };
                    hook?: boolean;
                    kind?: string;
                    name?: string;
                    namespace?: string;
                    requiresDeletionConfirmation?: boolean;
                    requiresPruning?: boolean;
                    status?: string;
                    /** Format: int64 */
                    syncWave?: number;
                    version?: string;
                }[];
                /** Format: int64 */
                resourcesCount?: number;
            };
        };
        /** @description ApplicationSetGenerateRequest is the body of POST /api/v1/applicationsets/generate. */
        ApplicationSetGenerateRequest: {
            applicationSet: components["schemas"]["ApplicationSet"];
        };
        /** @description ApplicationSetGenerateResponse lists the Applications an ApplicationSet would generate. */
        ApplicationSetGenerateResponse: {
            applications: components["schemas"]["StoredApplication"][];
        };
        /** @description ApplicationSet list. */
        ApplicationSetList: {
            apiVersion?: string;
            items: components["schemas"]["StoredApplicationSet"][];
            kind?: string;
            metadata?: components["schemas"]["ListMeta"];
        };
        /** @description ApplicationSetTree is the response of GET /api/v1/applicationsets/{name}/resource-tree. */
        ApplicationSetTree: {
            nodes: components["schemas"]["ApplicationSetTreeNode"][];
        };
        /** @description ApplicationSetTreeInfo is a name/value pair shown with a node. */
        ApplicationSetTreeInfo: {
            name: string;
            value: string;
        };
        /** @description ApplicationSetTreeNode is one generated Application in the ApplicationSet resource tree. */
        ApplicationSetTreeNode: {
            group?: string;
            health?: components["schemas"]["ApplicationStatusResourcesItemHealth"];
            /** @description Info carries the sync status of the Application ("Sync Status"). */
            info?: components["schemas"]["ApplicationSetTreeInfo"][];
            kind: string;
            name: string;
            namespace?: string;
            parentRefs?: components["schemas"]["ApplicationSetTreeRef"][];
            version?: string;
        };
        /** @description ApplicationSetTreeRef references a node's parent (the ApplicationSet). */
        ApplicationSetTreeRef: {
            group?: string;
            kind: string;
            name: string;
            namespace?: string;
            uid?: string;
            version?: string;
        };
        /** @description ApplicationSetWatchEvent is the data of one event of GET /api/v1/stream/applicationsets. */
        ApplicationSetWatchEvent: {
            object: components["schemas"]["StoredApplicationSet"];
            /**
             * @description Type is ADDED, MODIFIED or DELETED.
             * @enum {string}
             */
            type: "ADDED" | "MODIFIED" | "DELETED";
        };
        /** @description ApplicationSourceIntegrityCheck is one source integrity check and its problems. */
        ApplicationSourceIntegrityCheck: {
            name: string;
            problems?: string[];
        };
        /** @description ApplicationSourceIntegrityResult is the source integrity verification of a revision. */
        ApplicationSourceIntegrityResult: {
            checks?: components["schemas"]["ApplicationSourceIntegrityCheck"][];
        };
        ApplicationSources: components["schemas"]["ApplicationSpecSource"][];
        /** @description ApplicationSyncOptions wraps sync options as the Argo CD API does ({items: [...]}). When present in a sync request, Items replace the Application's sync options. */
        ApplicationSyncOptions: {
            items?: string[];
        };
        /** @description ApplicationSyncRequest is POST /api/v1/applications/{name}/sync. Its fields mirror Operation spec.sync. The Argo CD body fields name, appNamespace and project are accepted and must agree with the path and query. */
        ApplicationSyncRequest: {
            appNamespace?: string;
            dryRun?: boolean;
            /** @description Infos are recorded on the Operation. */
            infos?: components["schemas"]["ApplicationSpecInfoItem"][];
            /** @description Manifests are local manifests to sync instead of rendering (requires override; at most 1 MiB in total). */
            manifests?: string[];
            name?: string;
            project?: string;
            prune?: boolean;
            /** @description Resources restricts the sync to these resources (selective sync). */
            resources?: components["schemas"]["OperationSpecSyncResourcesItem"][];
            /** @description RetryStrategy replaces the Application's syncPolicy.retry for this operation. */
            retryStrategy?: components["schemas"]["ApplicationSpecSyncPolicyRetry"];
            /** @description Revision overrides the target revision of a single-source Application. */
            revision?: string;
            /** @description Revisions override the target revision of the sources at SourcePositions (1-based) of a multi-source Application. */
            revisions?: string[];
            /** @description Source and Sources override the Application's sources (Patrol addition; requires override). */
            source?: components["schemas"]["ApplicationSpecSource"];
            sourcePositions?: number[];
            sources?: components["schemas"]["ApplicationSources"];
            /** @description Strategy selects hook (default) or apply. */
            strategy?: components["schemas"]["OperationSpecSyncSyncStrategy"];
            /** @description SyncOptions, when present, replace the Application's sync options. */
            syncOptions?: components["schemas"]["ApplicationSyncOptions"];
        };
        /** @description ApplicationSyncWindow is a project sync window that applies to the Application. */
        ApplicationSyncWindow: {
            duration: string;
            kind: string;
            manualSync?: boolean;
            schedule: string;
            timeZone?: string;
        };
        /** @description ApplicationSyncWindowsResponse is GET /api/v1/applications/{name}/syncwindows. */
        ApplicationSyncWindowsResponse: {
            activeWindows?: components["schemas"]["ApplicationSyncWindow"][];
            assignedWindows?: components["schemas"]["ApplicationSyncWindow"][];
            /** @description CanSync is the manual sync decision now. */
            canSync: boolean;
            /**
             * Format: date-time
             * @description NextTransition is the next instant an assigned window opens or closes (Patrol addition; absent when none is known).
             */
            nextTransition?: string;
            /** @description Status is "Sync Allowed", "Manual Allowed" or "Sync Denied" (Patrol addition). */
            status?: string;
        };
        /** @description ApplicationTree is the resource tree of an Application: managed resources and their descendants, orphaned resources of the destination namespace (when the project monitors them) and the nodes running its pods. */
        ApplicationTree: {
            hosts?: components["schemas"]["HostInfo"][];
            nodes?: components["schemas"]["ResourceNode"][];
            orphanedNodes?: components["schemas"]["ResourceNode"][];
        };
        /** @description CanIResponse answers a can-i question. */
        CanIResponse: {
            /** @description Value is "yes" or "no". */
            value: string;
        };
        /**
         * @description Cluster registers a destination cluster. It replaces Argo CD's Secrets labeled
         *     argocd.argoproj.io/secret-type=cluster. Labels and annotations of the cluster (used by cluster
         *     generators and sharding) are the Cluster object's metadata. Credentials are never stored in the
         *     object: spec.config.credentialsRef names a Secret whose keys are the ClusterSecretKey* constants.
         */
        Cluster: {
            /**
             * @description APIVersion defines the versioned schema of this representation of an object.
             *     Servers should convert recognized schemas to the latest internal value, and
             *     may reject unrecognized values.
             *     More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources
             * @default cd.patrol.example/v1alpha1
             */
            apiVersion?: string;
            /**
             * @description Kind is a string value representing the REST resource this object represents.
             *     Servers may infer this from the endpoint the client submits requests to.
             *     Cannot be updated.
             *     In CamelCase.
             *     More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds
             */
            kind?: string;
            metadata?: components["schemas"]["ObjectMeta"];
            spec: components["schemas"]["ClusterSpec"];
            status?: components["schemas"]["ClusterStatus"];
        };
        /** @description ClusterConfigInput is the connection configuration of a cluster create or update. */
        ClusterConfigInput: {
            /** @description AWSAuthConfig authenticates with AWS IAM (EKS). */
            awsAuthConfig?: components["schemas"]["ClusterSpecConfigAwsAuthConfig"];
            /** @description BearerToken is a bearer token (write-only). */
            bearerToken?: string;
            /** @description DisableCompression disables response compression. */
            disableCompression?: boolean;
            /** @description ExecProviderConfig authenticates with an exec credential plugin. It is stored in the Cluster object (readable by whoever may read Cluster objects) but never returned by the API; do not put secrets in its args or env. */
            execProviderConfig?: components["schemas"]["ClusterSpecConfigExecProviderConfig"];
            password?: string;
            /** @description ProxyURL is an http, https or socks5 proxy for every request to the cluster. It must not contain credentials. */
            proxyUrl?: string;
            /** @description TLSClientConfig holds TLS settings; certificate and key data are write-only. */
            tlsClientConfig?: components["schemas"]["ClusterTLSClientConfigInput"];
            /** @description Username and Password are basic-authentication credentials (write-only). */
            username?: string;
        };
        /** @description ClusterCredentialsInfo tells which credentials a cluster has without revealing them. */
        ClusterCredentialsInfo: {
            /** @description HasBasicAuth is true when a username or password is stored. */
            hasBasicAuth?: boolean;
            /** @description HasBearerToken is true when a bearer token is stored. */
            hasBearerToken?: boolean;
            /** @description HasCAData is true when a custom CA certificate is stored. */
            hasCAData?: boolean;
            /** @description HasClientCertificate is true when a client certificate and key are stored. */
            hasClientCertificate?: boolean;
            /** @description HasExecProviderConfig is true when the cluster authenticates with an exec credential plugin (its configuration is not returned). */
            hasExecProviderConfig?: boolean;
        };
        /** @description ClusterInput is the cluster of a create or update request (Argo CD's Cluster message). Its config is write-only: credentials are stored in a Secret owned by the Cluster object and are never returned. */
        ClusterInput: {
            annotations?: {
                [key: string]: string;
            };
            /** @description ClusterResources allows cluster-scoped resources when Namespaces is set. */
            clusterResources?: boolean;
            /** @description Config is the connection configuration (write-only credentials). */
            config?: components["schemas"]["ClusterConfigInput"];
            /** @description Labels and Annotations replace those of the Cluster object. */
            labels?: {
                [key: string]: string;
            };
            /** @description Name of the cluster; defaults to the server URL. */
            name?: string;
            /** @description Namespaces restricts management to these namespaces. */
            namespaces?: string[];
            /** @description Project scopes the cluster to an AppProject. */
            project?: string;
            /** @description ResourceVersion, when set on an update, must match the stored Cluster (409 otherwise). The controller's status writes move it too: updatedFields confines a write without it. */
            resourceVersion?: string;
            /** @description Server is the API server URL (http or https). It identifies the cluster and cannot be changed by an update. */
            server: string;
            /** @description Shard pins the cluster to a controller shard; a negative value means unset. */
            shard?: number;
        };
        /** @description Cluster list. */
        ClusterList: {
            apiVersion?: string;
            items: components["schemas"]["StoredCluster"][];
            kind?: string;
            metadata?: components["schemas"]["ListMeta"];
        };
        /** @description ClusterTLSClientConfigInput holds the TLS settings of a cluster create or update. */
        ClusterTLSClientConfigInput: {
            /** @description CAData is the PEM CA bundle (base64 in JSON; write-only). */
            caData?: string;
            /** @description CertData and KeyData are the PEM client certificate and key (base64 in JSON; write-only). */
            certData?: string;
            /** @description Insecure skips server certificate verification. */
            insecure?: boolean;
            keyData?: string;
            /** @description ServerName is used for SNI and certificate verification. */
            serverName?: string;
        };
        /** @description CreateAccountTokenRequest is POST /api/v1/account/{name}/token. */
        CreateAccountTokenRequest: {
            /** @description ExpiresIn is the lifetime in seconds; 0 creates a token without expiry. */
            expiresIn?: number;
            /** @description ID is the token ID; empty generates a UUID. */
            id?: string;
        };
        /** @description DexConfig is the public part of dex.config. */
        DexConfig: {
            connectors?: components["schemas"]["DexConnector"][];
        };
        /** @description DexConnector names one Dex connector. */
        DexConnector: {
            name: string;
            type?: string;
        };
        /** @description Empty is a request or response without fields ({}). */
        Empty: Record<string, never>;
        Event: {
            action?: string;
            apiVersion?: string;
            count?: number;
            /** Format: date-time */
            eventTime?: string;
            /** Format: date-time */
            firstTimestamp?: string;
            involvedObject: components["schemas"]["ObjectReference"];
            kind?: string;
            /** Format: date-time */
            lastTimestamp?: string;
            message?: string;
            metadata: components["schemas"]["ObjectMeta"];
            reason?: string;
            related?: components["schemas"]["ObjectReference"];
            reportingComponent: string;
            reportingInstance: string;
            series?: components["schemas"]["EventSeries"];
            source?: components["schemas"]["EventSource"];
            type?: string;
        };
        EventList: {
            apiVersion?: string;
            items: components["schemas"]["Event"][];
            kind?: string;
            metadata?: components["schemas"]["ListMeta"];
        };
        EventSeries: {
            count?: number;
            /** Format: date-time */
            lastObservedTime?: string;
        };
        EventSource: {
            component?: string;
            host?: string;
        };
        /** @description GnuPGPublicKey is a GnuPG public key trusted for commit verification. */
        GnuPGPublicKey: {
            /** @description Fingerprint is the full fingerprint (upper-case hex). */
            fingerprint?: string;
            /** @description KeyData is the ASCII-armored public key (required on create; returned by get, never by list). */
            keyData?: string;
            /** @description KeyID is the 16-hex-digit key ID (upper case). */
            keyID?: string;
            /** @description Owner is the primary identity. */
            owner?: string;
            /** @description SubType is the key algorithm and size, e.g. "rsa4096" or "ed25519". */
            subType?: string;
            /** @description Trust is the trust level the repo-server assigns ("ultimate": every configured key is trusted). */
            trust?: string;
        };
        /** @description GnuPGPublicKeyCreateResponse reports the keys added and the IDs of keys already configured. */
        GnuPGPublicKeyCreateResponse: {
            created: components["schemas"]["GnuPGPublicKeyList"];
            skipped?: string[];
        };
        /** @description GnuPGPublicKeyList is a list of GnuPG public keys. */
        GnuPGPublicKeyList: {
            items: components["schemas"]["GnuPGPublicKey"][];
        };
        /** @description GoogleAnalyticsConfig is ga.*. */
        GoogleAnalyticsConfig: {
            anonymizeUsers?: boolean;
            trackingID?: string;
        };
        /** @description Help is help.*. */
        Help: {
            binaryUrls?: {
                [key: string]: string;
            };
            chatText?: string;
            chatUrl?: string;
        };
        /** @description HostInfo describes a node running pods of the Application. */
        HostInfo: {
            labels?: {
                [key: string]: string;
            };
            name?: string;
            resourcesInfo?: components["schemas"]["HostResourceInfo"][];
            systemInfo?: components["schemas"]["NodeSystemInfo"];
        };
        /** @description HostResourceInfo is the capacity and the requests of one resource on a node. */
        HostResourceInfo: {
            capacity?: number;
            requestedByApp?: number;
            requestedByNeighbors?: number;
            resourceName?: string;
        };
        /** @description InfoItem is a name/value pair shown for a resource. */
        InfoItem: {
            name?: string;
            value?: string;
        };
        /** @description KnownTypeField declares the type of a CRD field. */
        KnownTypeField: {
            field?: string;
            type?: string;
        };
        /** @description KustomizeOptions are global kustomize build options. */
        KustomizeOptions: {
            buildOptions?: string;
        };
        /** @description LinkInfo is one evaluated deep link (application.links, resource.links, project.links in patrol-cm). */
        LinkInfo: {
            description?: string;
            iconClass?: string;
            title: string;
            url: string;
        };
        /** @description LinksResponse is the response of the deep link routes (GET /api/v1/applications/{name}/links, .../resource/links, /api/v1/projects/{name}/links). */
        LinksResponse: {
            items: components["schemas"]["LinkInfo"][];
        };
        ListMeta: {
            continue?: string;
            remainingItemCount?: number;
            resourceVersion?: string;
            selfLink?: string;
            shardInfo?: components["schemas"]["ShardInfo"];
        };
        /** @description LoadBalancerIngress is one load balancer ingress point. */
        LoadBalancerIngress: {
            hostname?: string;
            ip?: string;
        };
        /** @description LogEntry is one log line. The final entry of a finished stream has Last set and no content. Content is untrusted text from the container. */
        LogEntry: {
            content: string;
            last: boolean;
            podName: string;
            /** Format: date-time */
            timeStamp?: string;
            timeStampStr?: string;
        };
        /** @description LogoutResponse is returned by DELETE /api/v1/session. */
        LogoutResponse: {
            /** @description LogoutURL is the identity provider's end-session URL for SSO sessions, when oidc.config.logoutURL is configured. */
            logoutURL?: string;
        };
        ManagedFieldsEntry: {
            apiVersion?: string;
            fieldsType?: string;
            fieldsV1?: Record<string, never>;
            manager?: string;
            operation?: string;
            subresource?: string;
            /** Format: date-time */
            time?: string;
        };
        /** @description ManagedResourcesResponse lists managed resources (hooks excluded). */
        ManagedResourcesResponse: {
            items: components["schemas"]["ResourceDiff"][];
        };
        NodeSwapStatus: {
            capacity?: number;
        };
        NodeSystemInfo: {
            architecture: string;
            bootID: string;
            containerRuntimeVersion: string;
            kernelVersion: string;
            kubeProxyVersion: string;
            kubeletVersion: string;
            machineID: string;
            operatingSystem: string;
            osImage: string;
            swap?: components["schemas"]["NodeSwapStatus"];
            systemUUID: string;
        };
        /** @description NotificationService names one configured notification service. Service configuration (URLs, tokens, recipients) is never returned. */
        NotificationService: {
            name: string;
        };
        /** @description NotificationServiceList is the response of GET /api/v1/notifications/services. */
        NotificationServiceList: {
            items: components["schemas"]["NotificationService"][];
        };
        /** @description NotificationTemplate names one configured template. */
        NotificationTemplate: {
            name: string;
        };
        /** @description NotificationTemplateList is the response of GET /api/v1/notifications/templates. */
        NotificationTemplateList: {
            items: components["schemas"]["NotificationTemplate"][];
        };
        /** @description NotificationTrigger names one configured trigger. */
        NotificationTrigger: {
            /** @description Description joins the distinct descriptions of the trigger's conditions (Patrol addition; empty when none is set). */
            description?: string;
            name: string;
        };
        /** @description NotificationTriggerList is the response of GET /api/v1/notifications/triggers. */
        NotificationTriggerList: {
            items: components["schemas"]["NotificationTrigger"][];
        };
        /** @description OIDCClaim is one requested ID token claim. */
        OIDCClaim: {
            essential?: boolean;
            value?: string;
            values?: string[];
        };
        /** @description OIDCConfig is the public part of oidc.config (never secrets). */
        OIDCConfig: {
            cliClientID?: string;
            clientID?: string;
            enablePKCEAuthentication?: boolean;
            idTokenClaims?: {
                [key: string]: components["schemas"]["OIDCClaim"];
            };
            issuer?: string;
            name?: string;
            scopes?: string[];
        };
        /** @description OIDCExchangeRequest exchanges an identity provider ID token (obtained by the CLI with the authorization-code + PKCE flow against the CLI client) for a Patrol session. */
        OIDCExchangeRequest: {
            /** @description AccessToken is required when oidc.config.enableUserInfoGroups is set: groups are then read from the userinfo endpoint with it. */
            accessToken?: string;
            idToken: string;
        };
        ObjectMeta: {
            annotations?: {
                [key: string]: string;
            };
            /** Format: date-time */
            creationTimestamp?: string;
            deletionGracePeriodSeconds?: number;
            /** Format: date-time */
            deletionTimestamp?: string;
            finalizers?: string[];
            generateName?: string;
            generation?: number;
            labels?: {
                [key: string]: string;
            };
            managedFields?: components["schemas"]["ManagedFieldsEntry"][];
            name?: string;
            namespace?: string;
            ownerReferences?: components["schemas"]["OwnerReference"][];
            resourceVersion?: string;
            selfLink?: string;
            uid?: string;
        };
        ObjectReference: {
            apiVersion?: string;
            fieldPath?: string;
            kind?: string;
            name?: string;
            namespace?: string;
            resourceVersion?: string;
            uid?: string;
        };
        /**
         * @description Operation is one sync or rollback request for an Application and the record of its execution.
         *     It replaces Argo CD's Application.operation field and status.operationState.syncResult (ADR-0011).
         *
         *     The spec is immutable after creation except spec.cancel, which may only change from false to true.
         *     Clients should change metadata and spec.cancel with a patch (merge or server-side apply).
         *     Immutability is enforced by comparing the stored spec, and Go clients omit empty values: scalar
         *     fields of spec.sync and spec.initiatedBy are defaulted or compared as their zero value, so a full
         *     update of an Operation written with explicit `false`/`""`/`0` works, but one written with an explicit
         *     empty list or object (e.g. `syncOptions: []`) is rejected by a full update from a Go client.
         *
         *     A ValidatingAdmissionPolicy shipped with the CRDs (OperationAdmissionPolicyName, in deploy/crds)
         *     additionally requires spec.initiatedBy to name the requesting user (or be empty) and to not be
         *     automated unless the requester holds the OperationInitiatorVerb on operations, and requires the
         *     LabelKeyOperationApplication label, when present, to equal spec.application.
         */
        Operation: {
            /**
             * @description APIVersion defines the versioned schema of this representation of an object.
             *     Servers should convert recognized schemas to the latest internal value, and
             *     may reject unrecognized values.
             *     More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources
             * @default cd.patrol.example/v1alpha1
             */
            apiVersion?: string;
            /**
             * @description Kind is a string value representing the REST resource this object represents.
             *     Servers may infer this from the endpoint the client submits requests to.
             *     Cannot be updated.
             *     In CamelCase.
             *     More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds
             */
            kind?: string;
            metadata?: components["schemas"]["ObjectMeta"];
            /** @description Spec is the requested operation. */
            spec: {
                /** @description Application is the name of the Application, in the Operation's namespace, to operate on. */
                application: string;
                /**
                 * @description Cancel requests cancellation: the operation moves to Terminating, running hooks are cleaned up
                 *     per policy, no new tasks start, and it ends Cancelled. Can only change from false to true.
                 */
                cancel?: boolean;
                /** @description Info holds informational name/value pairs (e.g. the reason for a manual sync). */
                info?: components["schemas"]["ApplicationSpecInfoItem"][];
                /**
                 * @description InitiatedBy records who requested the operation. patrol-server sets it from verified token claims
                 *     and the controller sets automated; other Kubernetes users may only leave it empty or name
                 *     themselves (enforced by the OperationAdmissionPolicyName ValidatingAdmissionPolicy). Compared
                 *     field by field, with absent fields as their zero value, so Go clients can update the Operation.
                 * @default {}
                 */
                initiatedBy?: {
                    /** @description Automated is true if the controller requested the operation (automated sync or self-heal). */
                    automated?: boolean;
                    /** @description Username of the user who requested the operation. */
                    username?: string;
                };
                /** @description Retry controls retries of failed attempts. Defaults to the Application's syncPolicy.retry. */
                retry?: components["schemas"]["ApplicationSpecSyncPolicyRetry"];
                /** @description Rollback requests a sync to the sources and revisions of a status.history entry. */
                rollback?: {
                    /**
                     * Format: int64
                     * @description ID of the status.history entry of the Application.
                     */
                    id: number;
                };
                /** @description Sync requests a sync. Also carries the sync options of a rollback. */
                sync?: {
                    /**
                     * Format: int64
                     * @description SelfHealAttemptsCount is the number of self-heal attempts that preceded this operation.
                     * @default 0
                     */
                    autoHealAttemptsCount?: number;
                    /**
                     * @description DryRun performs a server-side dry run without changing resources.
                     * @default false
                     */
                    dryRun?: boolean;
                    /**
                     * @description Manifests replace rendering with these YAML/JSON documents (local sync for development).
                     *     Each document is at most 1MiB and the total at most 1MiB.
                     */
                    manifests?: string[];
                    /**
                     * @description Prune deletes resources that are no longer desired.
                     * @default false
                     */
                    prune?: boolean;
                    /** @description Resources restricts the sync to these resources (selective sync). Empty means all. */
                    resources?: components["schemas"]["OperationSpecSyncResourcesItem"][];
                    /**
                     * @description Revision overrides the target revision of a single-source Application.
                     * @default
                     */
                    revision?: string;
                    /** @description Revisions override the target revision of each source of a multi-source Application. */
                    revisions?: string[];
                    /** @description Source overrides the Application source (single-source). */
                    source?: components["schemas"]["ApplicationSpecSource"];
                    /** @description Sources override the Application sources (multi-source). */
                    sources?: components["schemas"]["ApplicationSpecSource"][];
                    /** @description SyncOptions are per-operation sync options, merged over the Application's. */
                    syncOptions?: string[];
                    syncStrategy?: components["schemas"]["OperationSpecSyncSyncStrategy"];
                };
            };
            /** @description Status is the execution record, written by the controller. */
            status?: {
                /** @description Conditions are the current Operation conditions (Admitted, Planned, Completed, Retrying). */
                conditions?: {
                    /**
                     * Format: date-time
                     * @description lastTransitionTime is the last time the condition transitioned from one status to another.
                     *     This should be when the underlying condition changed.  If that is not known, then using the time when the API field changed is acceptable.
                     */
                    lastTransitionTime: string;
                    /**
                     * @description message is a human readable message indicating details about the transition.
                     *     This may be an empty string.
                     */
                    message: string;
                    /**
                     * Format: int64
                     * @description observedGeneration represents the .metadata.generation that the condition was set based upon.
                     *     For instance, if .metadata.generation is currently 12, but the .status.conditions[x].observedGeneration is 9, the condition is out of date
                     *     with respect to the current state of the instance.
                     */
                    observedGeneration?: number;
                    /**
                     * @description reason contains a programmatic identifier indicating the reason for the condition's last transition.
                     *     Producers of specific condition types may define expected values and meanings for this field,
                     *     and whether the values are considered a guaranteed API.
                     *     The value should be a CamelCase string.
                     *     This field may not be empty.
                     */
                    reason: string;
                    /**
                     * @description status of the condition, one of True, False, Unknown.
                     * @enum {string}
                     */
                    status: "True" | "False" | "Unknown";
                    /** @description type of condition in CamelCase or in foo.example.com/CamelCase. */
                    type: string;
                }[];
                /**
                 * Format: date-time
                 * @description FinishedAt is when the operation reached a terminal phase.
                 */
                finishedAt?: string;
                /** @description Message describes the current phase (typically an error). */
                message?: string;
                /**
                 * Format: int64
                 * @description ObservedGeneration is the metadata.generation the status reflects.
                 */
                observedGeneration?: number;
                /**
                 * @description Phase of the operation.
                 * @enum {string}
                 */
                phase?: "Pending" | "Running" | "Terminating" | "Succeeded" | "Failed" | "Error" | "Cancelled" | "Rejected";
                /** @description Plan is the persisted execution plan; a restarted controller resumes from it. */
                plan?: {
                    /** @description Destination is the resolved destination (server URL resolved from a cluster name). */
                    destination?: components["schemas"]["ApplicationSpecDestination"];
                    /** @description RenderedInputDigest is a digest of every rendering input, used to detect drift on resume. */
                    renderedInputDigest?: string;
                    /** @description ResolvedRevisions are the resolved revision of each source, persisted before any mutation. */
                    resolvedRevisions?: string[];
                    /** @description Warnings are planning warnings (e.g. destructive changes). */
                    warnings?: string[];
                };
                /** @description Progress is the execution progress, persisted after every wave. */
                progress?: {
                    /**
                     * Format: int64
                     * @description CompletedTasks is the number of completed tasks.
                     */
                    completedTasks?: number;
                    /**
                     * Format: date-time
                     * @description StartedAt is when the current wave started.
                     */
                    startedAt?: string;
                    /** @description SyncPhase is the phase being executed. */
                    syncPhase?: string;
                    /**
                     * Format: int64
                     * @description TotalTasks is the number of planned tasks.
                     */
                    totalTasks?: number;
                    /**
                     * Format: int64
                     * @description Wave is the wave being executed.
                     */
                    wave?: number;
                };
                /**
                 * Format: int64
                 * @description RetryCount is the number of retries performed.
                 */
                retryCount?: number;
                /**
                 * Format: date-time
                 * @description StartedAt is when execution started.
                 */
                startedAt?: string;
                /** @description SyncResult holds per-resource results and the synced revisions/sources. */
                syncResult?: {
                    /** @description ManagedNamespaceMetadata is the namespace metadata applied by the sync. */
                    managedNamespaceMetadata?: components["schemas"]["ApplicationSpecSyncPolicyManagedNamespaceMetadata"];
                    /** @description Resources are the per-resource results. */
                    resources?: {
                        group: string;
                        /**
                         * @description HookPhase is the state of the hook (or of a resource with a health-based phase).
                         * @enum {string}
                         */
                        hookPhase?: "Pending" | "Running" | "Terminating" | "Succeeded" | "Failed" | "Error" | "Cancelled" | "Rejected";
                        /** @description HookType is set for hooks. */
                        hookType?: string;
                        /** @description Images used by the resource. */
                        images?: string[];
                        kind: string;
                        /** @description Message is an informational or error message. */
                        message?: string;
                        name: string;
                        namespace: string;
                        /**
                         * @description Status is the final result; empty while pending and for hooks.
                         * @enum {string}
                         */
                        status?: "Synced" | "SyncFailed" | "Pruned" | "PruneSkipped";
                        /** @description SyncPhase is the sync phase the result was recorded in. */
                        syncPhase?: string;
                        version: string;
                    }[];
                    /** @description Revision is the revision synced (single-source). */
                    revision?: string;
                    /** @description Revisions are the revisions synced for each source (multi-source). */
                    revisions?: string[];
                    /** @description Source is the source synced (single-source). */
                    source?: components["schemas"]["ApplicationSpecSource"];
                    /** @description Sources are the sources synced (multi-source). */
                    sources?: components["schemas"]["ApplicationSpecSource"][];
                };
            };
        };
        /** @description Operation list. */
        OperationList: {
            apiVersion?: string;
            items: components["schemas"]["StoredOperation"][];
            kind?: string;
            metadata?: components["schemas"]["ListMeta"];
        };
        /** @description OverrideIgnoreDiff lists fields ignored by diffing (or by refresh triggering). */
        OverrideIgnoreDiff: {
            jqPathExpressions?: string[];
            jsonPointers?: string[];
            managedFieldsManagers?: string[];
        };
        OwnerReference: {
            apiVersion: string;
            blockOwnerDeletion?: boolean;
            controller?: boolean;
            kind: string;
            name: string;
            uid: string;
        };
        /** @description Plugin is a config management plugin served by a repo-server sidecar. */
        Plugin: {
            name: string;
        };
        /** @description PluginSettings is the response of GET /api/v1/settings/plugins. */
        PluginSettings: {
            plugins?: components["schemas"]["Plugin"][];
        };
        /** @description Problem is an RFC 9457 problem details object (application/problem+json). */
        Problem: {
            /**
             * @description Code is the stable error identifier.
             * @enum {string}
             */
            code?: "not_found" | "permission_denied" | "invalid_argument" | "conflict" | "already_exists" | "failed_precondition" | "unauthenticated" | "unavailable" | "internal" | "resource_exhausted";
            /** @description Detail is a human-readable explanation specific to this occurrence. */
            detail?: string;
            /** @description Instance identifies the request (the request path). */
            instance?: string;
            /** @description RequestID correlates the response with server logs and audit records. */
            requestId?: string;
            /** @description Status is the HTTP status code. */
            status: number;
            /** @description Title is the reason phrase of Status. */
            title: string;
            /** @description Type is "urn:patrol-cd:problem:<code>". */
            type?: string;
        };
        /** @description ProjectCreateRequest is the body of POST /api/v1/projects (Argo CD's ProjectCreateRequest). status is ignored, and so is the deprecated spec.roles[].jwtTokens: tokens are created with the token route only. */
        ProjectCreateRequest: {
            project: components["schemas"]["AppProject"];
            /** @description Upsert replaces the spec of an existing project (needs projects, update). */
            upsert?: boolean;
        };
        /** @description ProjectDetailed is the response of GET /api/v1/projects/{name}/detailed. */
        ProjectDetailed: {
            /** @description Clusters are the clusters scoped to the project that the caller may get. */
            clusters: components["schemas"]["RegisteredCluster"][];
            /** @description GlobalProjects are the global projects it inherits from (status removed). */
            globalProjects: components["schemas"]["StoredAppProject"][];
            /** @description Project is the effective project: the stored project with the lists of its global projects appended. */
            project: components["schemas"]["StoredAppProject"];
            /** @description Repositories are the repositories scoped to the project that the caller may get. */
            repositories: components["schemas"]["StoredRepository"][];
        };
        /** @description ProjectGlobalProjectsResponse is the response of GET /api/v1/projects/{name}/globalprojects. */
        ProjectGlobalProjectsResponse: {
            items: components["schemas"]["StoredAppProject"][];
        };
        /** @description ProjectSyncWindowsResponse is the response of GET /api/v1/projects/{name}/syncwindows: the project's windows active now (its own windows, without global projects). */
        ProjectSyncWindowsResponse: {
            windows: components["schemas"]["AppProjectSpecSyncWindowsItem"][];
        };
        /** @description ProjectTokenCreateRequest is POST /api/v1/projects/{project}/roles/{role}/token. */
        ProjectTokenCreateRequest: {
            /** @description Description is accepted for compatibility and not stored. */
            description?: string;
            /** @description ExpiresIn is the lifetime in seconds; 0 creates a token without expiry. */
            expiresIn?: number;
            /** @description ID is the token ID (jti); empty generates a UUID. It must be unique per role. */
            id?: string;
        };
        /** @description ProjectTokenResponse carries a new project role token. The token is returned only by this response; the project records its ID and times only. */
        ProjectTokenResponse: {
            expiresAt?: number;
            /** @description ID is the token's jti. */
            id?: string;
            /** @description IssuedAt and ExpiresAt are Unix timestamps (seconds); ExpiresAt 0 means no expiry. */
            issuedAt?: number;
            token: string;
        };
        /** @description ProjectUpdateRequest is PUT /api/v1/projects/{name}. metadata.name of the project must be empty or the path name; metadata.resourceVersion, when set, must match (409 otherwise). Only the spec, labels and annotations are written: status and the deprecated spec.roles[].jwtTokens keep their stored values. */
        ProjectUpdateRequest: {
            project: components["schemas"]["AppProject"];
        };
        /** @description RegisteredCluster is a registered destination cluster as returned by the API: the Cluster object with its connection configuration sanitized (spec.config.execProviderConfig removed, as its args and env may hold secrets) and flags telling which credentials are stored. Credential values are never returned. */
        RegisteredCluster: {
            apiVersion?: string;
            /** @description Credentials reports which credentials are configured. */
            credentials: components["schemas"]["ClusterCredentialsInfo"];
            /** @description InCluster is true for the implicit in-cluster cluster that no Cluster object registers (cluster.inClusterEnabled). */
            inCluster?: boolean;
            kind?: string;
            /** @description Metadata is the Cluster object's metadata (empty name for the implicit in-cluster cluster). The kubectl last-applied-configuration annotation is removed. */
            metadata: components["schemas"]["ObjectMeta"];
            /** @description Spec is the cluster configuration without spec.config.execProviderConfig. */
            spec: components["schemas"]["ClusterSpec"];
            /** @description Status is the observed connection state, server version, cache information and assigned shard. */
            status?: components["schemas"]["ClusterStatus"];
        };
        /** @description RegisteredClusterList is the response of GET /api/v1/clusters. */
        RegisteredClusterList: {
            items: components["schemas"]["RegisteredCluster"][];
            metadata: components["schemas"]["ListMeta"];
        };
        /**
         * @description Repository registers one git, Helm or OCI repository and how to access it. It replaces Argo CD's
         *     Secrets labeled argocd.argoproj.io/secret-type=repository (and repository-write). Credentials are
         *     never stored in the object: spec.credentialsRef names a Secret in the same namespace whose keys are
         *     the RepositorySecretKey* constants.
         */
        Repository: {
            /**
             * @description APIVersion defines the versioned schema of this representation of an object.
             *     Servers should convert recognized schemas to the latest internal value, and
             *     may reject unrecognized values.
             *     More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources
             * @default cd.patrol.example/v1alpha1
             */
            apiVersion?: string;
            /**
             * @description Kind is a string value representing the REST resource this object represents.
             *     Servers may infer this from the endpoint the client submits requests to.
             *     Cannot be updated.
             *     In CamelCase.
             *     More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds
             */
            kind?: string;
            metadata?: components["schemas"]["ObjectMeta"];
            spec: components["schemas"]["RepositorySpec"];
            status?: components["schemas"]["RepositoryStatus"];
        };
        /** @description RepositoryApp is one application directory found in a repository. */
        RepositoryApp: {
            path: string;
            /** @description Type is the detected source type (Helm, Kustomize, Directory, Plugin). */
            type: string;
        };
        /** @description RepositoryAppDetails are the tool-specific details of an application source. */
        RepositoryAppDetails: {
            directory?: components["schemas"]["RepositoryDirectoryAppSpec"];
            helm?: components["schemas"]["RepositoryHelmAppSpec"];
            kustomize?: components["schemas"]["RepositoryKustomizeAppSpec"];
            plugin?: components["schemas"]["RepositoryPluginAppSpec"];
            type: string;
        };
        /** @description RepositoryAppDetailsQuery is the body of POST /api/v1/repositories/{repo}/appdetails. */
        RepositoryAppDetailsQuery: {
            /** @description AppName ("name" or "namespace/name") and AppProject name the Application. */
            appName?: string;
            appProject?: string;
            /** @description Source is the application source to inspect; source.repoURL defaults to the path's repository and must name the same repository. */
            source: components["schemas"]["ApplicationSpecSource"];
            /** @description SourceIndex and VersionID select a source of a multi-source history entry. */
            sourceIndex?: number;
            versionId?: number;
        };
        /** @description RepositoryAppList is the response of GET /api/v1/repositories/{repo}/apps. */
        RepositoryAppList: {
            items: components["schemas"]["RepositoryApp"][];
        };
        /** @description RepositoryCertificate is a TLS certificate or an SSH known host entry for a repository server. */
        RepositoryCertificate: {
            /** @description CertData is the PEM data (https) or the base64 public key (ssh). It is required on create and returned by create and delete, never by list. */
            certData?: string;
            /** @description CertInfo is the subject DN (https) or "SHA256:<fingerprint>" (ssh). */
            certInfo?: string;
            /** @description CertSubType is the SSH key type (e.g. "ssh-ed25519") or the lower-cased public key algorithm of a TLS certificate ("rsa", "ecdsa", "ed25519"; "invalid" when the stored PEM data cannot be decoded). */
            certSubType?: string;
            /** @description CertType is "https" or "ssh". */
            certType: string;
            /** @description ServerName is the host name (TLS), or the known_hosts host field, e.g. "github.com" or "[git.example.com]:2222" (SSH). */
            serverName: string;
        };
        /** @description RepositoryCertificateList is a list of certificates. */
        RepositoryCertificateList: {
            items: components["schemas"]["RepositoryCertificate"][];
        };
        /**
         * @description RepositoryCredential is a credential template applied to every repository whose URL starts with
         *     spec.urlPrefix and that has no credentials of its own (longest matching prefix wins). It replaces
         *     Argo CD's Secrets labeled argocd.argoproj.io/secret-type=repo-creds (and repo-write-creds).
         */
        RepositoryCredential: {
            /**
             * @description APIVersion defines the versioned schema of this representation of an object.
             *     Servers should convert recognized schemas to the latest internal value, and
             *     may reject unrecognized values.
             *     More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources
             * @default cd.patrol.example/v1alpha1
             */
            apiVersion?: string;
            /**
             * @description Kind is a string value representing the REST resource this object represents.
             *     Servers may infer this from the endpoint the client submits requests to.
             *     Cannot be updated.
             *     In CamelCase.
             *     More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds
             */
            kind?: string;
            metadata?: components["schemas"]["ObjectMeta"];
            spec: components["schemas"]["RepositoryCredentialSpec"];
            /** @description Status is the observed state of the template. */
            status?: components["schemas"]["RepositoryStatus"];
        };
        /** @description RepositoryCredentialInfo is a credential template as returned by the API (never its credential values). */
        RepositoryCredentialInfo: {
            apiVersion?: string;
            /** @description HasCredentials is true when the template has credentials of its own (a credentials Secret, an SCMProvider or Azure workload identity). */
            hasCredentials: boolean;
            kind?: string;
            metadata: components["schemas"]["ObjectMeta"];
            spec: components["schemas"]["RepositoryCredentialSpec"];
            status?: components["schemas"]["RepositoryStatus"];
            /** @description Username is the stored user name (not a secret; upstream lists it). */
            username?: string;
        };
        /** @description RepositoryCredentialInfoList is the response of GET /api/v1/repocreds and /api/v1/write-repocreds. */
        RepositoryCredentialInfoList: {
            items: components["schemas"]["RepositoryCredentialInfo"][];
            metadata: components["schemas"]["ListMeta"];
        };
        /** @description RepositoryCredential list. */
        RepositoryCredentialList: {
            apiVersion?: string;
            items: components["schemas"]["StoredRepositoryCredential"][];
            kind?: string;
            metadata?: components["schemas"]["ListMeta"];
        };
        /** @description RepositoryCredentialWriteRequest is the body of credential template create and update requests: a RepositoryCredential object plus write-only credentials (see RepositoryWriteRequest for the metadata, credentialsRef and status rules). */
        RepositoryCredentialWriteRequest: {
            apiVersion?: string;
            /** @description Credentials are write-only credential material. */
            credentials?: components["schemas"]["RepositoryCredentials"];
            kind?: string;
            /** @description Metadata of the object; only resourceVersion is used (optimistic concurrency). */
            metadata?: components["schemas"]["ObjectMeta"];
            /** @description Spec is the template. */
            spec: components["schemas"]["RepositoryCredentialSpec"];
            /** @description Status is accepted and ignored. */
            status?: components["schemas"]["RepositoryStatus"];
        };
        /** @description RepositoryCredentials is write-only credential material of a repository or credential template. The server stores it in a Secret it manages (labelled with the cd.patrol.example/secret-type of the object, owned by the object and referenced by spec.credentialsRef) and never returns it. In an update, a nil Credentials leaves the stored credentials unchanged, while a non-nil one replaces every recognized credential key (keys left empty are removed; keys the server does not recognize are kept). */
        RepositoryCredentials: {
            /** @description AzureServicePrincipalClientSecret is the client secret of spec.azureServicePrincipal. */
            azureServicePrincipalClientSecret?: string;
            /** @description BearerToken for HTTPS bearer authentication (Bitbucket Data Center). */
            bearerToken?: string;
            /** @description GCPServiceAccountKey is a Google service account key (JSON). */
            gcpServiceAccountKey?: string;
            /** @description GitHubAppPrivateKey is the PEM private key of the GitHub App in spec.githubApp. */
            githubAppPrivateKey?: string;
            /** @description Password or personal access token for HTTPS basic authentication. */
            password?: string;
            /** @description SSHPrivateKey is a PEM or OpenSSH private key without passphrase. */
            sshPrivateKey?: string;
            /** @description TLSClientCertData and TLSClientCertKey are a PEM client certificate and its key. */
            tlsClientCertData?: string;
            tlsClientCertKey?: string;
            /** @description Username for HTTPS (git, Helm, OCI) basic authentication. */
            username?: string;
        };
        /** @description RepositoryDirectoryAppSpec describes a plain directory. */
        RepositoryDirectoryAppSpec: Record<string, never>;
        /** @description RepositoryHelmAppSpec describes a Helm chart. */
        RepositoryHelmAppSpec: {
            fileParameters?: components["schemas"]["ApplicationSpecSourceHelmFileParametersItem"][];
            name?: string;
            parameters?: components["schemas"]["ApplicationSpecSourceHelmParametersItem"][];
            valueFiles?: string[];
            values?: string;
        };
        /** @description RepositoryHelmChart is one chart of a Helm repository index. */
        RepositoryHelmChart: {
            name: string;
            versions?: string[];
        };
        /** @description RepositoryHelmChartList is the response of GET /api/v1/repositories/{repo}/helmcharts. */
        RepositoryHelmChartList: {
            items: components["schemas"]["RepositoryHelmChart"][];
        };
        /** @description RepositoryInfo is a Repository as returned by the API: the stored object, which never contains credential values, plus derived fields. */
        RepositoryInfo: {
            apiVersion?: string;
            /** @description ConnectionState is status.connectionState, or Unknown when the repository was not checked yet. */
            connectionState: components["schemas"]["RepositoryStatusConnectionState"];
            /** @description HasCredentials is true when the repository has credentials of its own (a credentials Secret, an SCMProvider or Azure workload identity). */
            hasCredentials: boolean;
            /** @description InheritedCredentials is true when the repository has no credentials of its own and a credential template of the same purpose matches its URL. */
            inheritedCredentials?: boolean;
            kind?: string;
            /** @description Metadata of the Repository object (managed fields removed). */
            metadata: components["schemas"]["ObjectMeta"];
            /** @description Spec is the repository configuration; spec.credentialsRef names the Secret holding its credentials. */
            spec: components["schemas"]["RepositorySpec"];
            /** @description Status holds the last recorded connection state and conditions. */
            status?: components["schemas"]["RepositoryStatus"];
        };
        /** @description RepositoryInfoList is the response of GET /api/v1/repositories and /api/v1/write-repositories. */
        RepositoryInfoList: {
            items: components["schemas"]["RepositoryInfo"][];
            metadata: components["schemas"]["ListMeta"];
        };
        /** @description RepositoryKustomizeAppSpec describes a kustomization. */
        RepositoryKustomizeAppSpec: {
            images?: string[];
        };
        /** @description Repository list. */
        RepositoryList: {
            apiVersion?: string;
            items: components["schemas"]["StoredRepository"][];
            kind?: string;
            metadata?: components["schemas"]["ListMeta"];
        };
        /** @description RepositoryPluginAppSpec describes a config management plugin source. */
        RepositoryPluginAppSpec: {
            parametersAnnouncement?: components["schemas"]["RepositoryPluginParameter"][];
        };
        /** @description RepositoryPluginParameter is a parameter announced by a config management plugin. */
        RepositoryPluginParameter: {
            array?: string[];
            collectionType?: string;
            itemType?: string;
            map?: {
                [key: string]: string;
            };
            name?: string;
            required?: boolean;
            string?: string;
            title?: string;
            tooltip?: string;
        };
        /** @description RepositoryRefs lists branches and tags (git) or tags (OCI). */
        RepositoryRefs: {
            branches?: string[];
            tags?: string[];
        };
        /** @description RepositoryWriteRequest is the body of repository create, update and validate requests: a Repository object (apiVersion and kind optional) plus write-only credentials. spec.credentialsRef is managed by the server: it must be empty on create and unchanged on update. metadata.resourceVersion, when set on an update, must match the stored object (409 conflict otherwise); metadata.name is derived from the URL and project on create. status is ignored. */
        RepositoryWriteRequest: {
            apiVersion?: string;
            /** @description Credentials are write-only credential material. */
            credentials?: components["schemas"]["RepositoryCredentials"];
            kind?: string;
            /** @description Metadata of the object; only resourceVersion is used (optimistic concurrency). */
            metadata?: components["schemas"]["ObjectMeta"];
            /** @description Spec is the repository configuration. */
            spec: components["schemas"]["RepositorySpec"];
            /** @description Status is accepted (so a fetched object can be sent back) and ignored. */
            status?: components["schemas"]["RepositoryStatus"];
        };
        /** @description ResourceAction is an action available for a resource. */
        ResourceAction: {
            disabled?: boolean;
            displayName?: string;
            iconClass?: string;
            name: string;
            params?: components["schemas"]["ResourceActionParam"][];
        };
        /** @description ResourceActionParam is an input declared by an action's discovery. */
        ResourceActionParam: {
            name: string;
        };
        /** @description ResourceActionParameter is a value passed to an action (actionParams[name] in Lua). */
        ResourceActionParameter: {
            name: string;
            value: string;
        };
        /** @description ResourceActionRunRequest runs an action on a resource of an Application's tree, or on the Application itself (group cd.patrol.example, kind Application, resourceName = the Application's name). */
        ResourceActionRunRequest: {
            action: string;
            resourceActionParameters?: components["schemas"]["ResourceActionParameter"][];
        };
        /** @description ResourceActionsListResponse lists the actions of a resource. */
        ResourceActionsListResponse: {
            actions: components["schemas"]["ResourceAction"][];
        };
        /** @description ResourceDiff is one managed resource with its desired and live states (JSON; core Secret data and sensitive annotations masked). "null" stands for a missing side. */
        ResourceDiff: {
            diff?: string;
            group?: string;
            hook?: boolean;
            kind?: string;
            liveState?: string;
            modified?: boolean;
            name?: string;
            namespace?: string;
            normalizedLiveState?: string;
            predictedLiveState?: string;
            resourceVersion?: string;
            targetState?: string;
        };
        /** @description ResourceNetworkingInfo is the networking metadata of a resource. */
        ResourceNetworkingInfo: {
            externalURLs?: string[];
            ingress?: components["schemas"]["LoadBalancerIngress"][];
            labels?: {
                [key: string]: string;
            };
            targetLabels?: {
                [key: string]: string;
            };
            targetRefs?: components["schemas"]["ResourceRef"][];
        };
        /** @description ResourceNode is one node of a resource tree. */
        ResourceNode: {
            /** Format: date-time */
            createdAt?: string;
            group?: string;
            health?: components["schemas"]["ApplicationStatusResourcesItemHealth"];
            images?: string[];
            info?: components["schemas"]["InfoItem"][];
            kind?: string;
            name?: string;
            namespace?: string;
            networkingInfo?: components["schemas"]["ResourceNetworkingInfo"];
            parentRefs?: components["schemas"]["ResourceRef"][];
            resourceVersion?: string;
            uid?: string;
            version?: string;
        };
        /** @description ResourceOverride is a resource.customizations entry. */
        ResourceOverride: {
            actions?: string;
            "health.lua"?: string;
            "health.lua.useOpenLibs"?: boolean;
            ignoreDifferences?: components["schemas"]["OverrideIgnoreDiff"];
            ignoreResourceUpdates?: components["schemas"]["OverrideIgnoreDiff"];
            knownTypeFields?: components["schemas"]["KnownTypeField"][];
        };
        /** @description ResourceRef identifies a resource in a resource tree. */
        ResourceRef: {
            group?: string;
            kind?: string;
            name?: string;
            namespace?: string;
            uid?: string;
            version?: string;
        };
        /**
         * @description SCMProvider is an SCM API endpoint and its credentials, webhook secret and commit status
         *     reporting settings (ADR-0010). Repositories, ApplicationSet generators and notifications reference
         *     it by name instead of carrying their own tokens.
         */
        SCMProvider: {
            /**
             * @description APIVersion defines the versioned schema of this representation of an object.
             *     Servers should convert recognized schemas to the latest internal value, and
             *     may reject unrecognized values.
             *     More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources
             * @default cd.patrol.example/v1alpha1
             */
            apiVersion?: string;
            /**
             * @description Kind is a string value representing the REST resource this object represents.
             *     Servers may infer this from the endpoint the client submits requests to.
             *     Cannot be updated.
             *     In CamelCase.
             *     More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds
             */
            kind?: string;
            metadata?: components["schemas"]["ObjectMeta"];
            spec: components["schemas"]["SCMProviderSpec"];
            status?: components["schemas"]["SCMProviderStatus"];
        };
        /** @description SCMProvider list. */
        SCMProviderList: {
            apiVersion?: string;
            items: components["schemas"]["StoredSCMProvider"][];
            kind?: string;
            metadata?: components["schemas"]["ListMeta"];
        };
        /** @description SCMProviderWriteRequest is the body of SCM provider create and update requests: an SCMProvider object. Secrets are referenced by name only (spec.caSecretRef, spec.auth.*, spec.webhook.secretRef); the API never accepts secret values for providers. status is ignored. */
        SCMProviderWriteRequest: {
            apiVersion?: string;
            kind?: string;
            /** @description Metadata: name (required on create, must match the path on update), labels, annotations and resourceVersion (optimistic concurrency on update). */
            metadata: components["schemas"]["ObjectMeta"];
            /** @description Spec is the provider configuration. */
            spec: components["schemas"]["SCMProviderSpec"];
            /** @description Status is accepted and ignored. */
            status?: components["schemas"]["SCMProviderStatus"];
        };
        /** @description ServerSideDiffResponse is the result of a server-side diff: per item, targetState is the predicted live state and liveState the normalized live state. */
        ServerSideDiffResponse: {
            items: components["schemas"]["ResourceDiff"][];
            modified: boolean;
        };
        /** @description SessionCreateRequest is the body of POST /api/v1/session (local login). */
        SessionCreateRequest: {
            password: string;
            username: string;
        };
        /** @description SessionResponse carries a Patrol session token. */
        SessionResponse: {
            token?: string;
        };
        /** @description Settings is the response of GET /api/v1/settings. Fields marked "authenticated" are only populated for logged-in callers (or when authentication is disabled); PasswordPattern only for logged-in callers. */
        Settings: {
            additionalUrls?: string[];
            anonymousEnabled?: boolean;
            appLabelKey?: string;
            appsInAnyNamespaceEnabled?: boolean;
            controllerNamespace?: string;
            dexConfig?: components["schemas"]["DexConfig"];
            execEnabled?: boolean;
            googleAnalytics?: components["schemas"]["GoogleAnalyticsConfig"];
            help?: components["schemas"]["Help"];
            hydratorEnabled?: boolean;
            impersonationEnabled?: boolean;
            installationID?: string;
            kustomizeOptions?: components["schemas"]["KustomizeOptions"];
            kustomizeVersions?: string[];
            oidcConfig?: components["schemas"]["OIDCConfig"];
            passwordPattern?: string;
            resourceOverrides?: {
                [key: string]: components["schemas"]["ResourceOverride"];
            };
            statusBadgeEnabled?: boolean;
            statusBadgeRootUrl?: string;
            syncWithReplaceAllowed?: boolean;
            trackingMethod?: string;
            uiBannerContent?: string;
            uiBannerPermanent?: boolean;
            uiBannerPosition?: string;
            uiBannerURL?: string;
            uiCssURL?: string;
            url?: string;
            userLoginsDisabled?: boolean;
        };
        ShardInfo: {
            selector: string;
        };
        /** @description AppProject as returned by the server (a stored object, or one the server rendered): metadata is always present. Request bodies use AppProject, whose metadata is optional. */
        StoredAppProject: WithRequired<components["schemas"]["AppProject"], "metadata">;
        /** @description Application as returned by the server (a stored object, or one the server rendered): metadata is always present. Request bodies use Application, whose metadata is optional. */
        StoredApplication: WithRequired<components["schemas"]["Application"], "metadata">;
        /** @description ApplicationSet as returned by the server (a stored object, or one the server rendered): metadata is always present. Request bodies use ApplicationSet, whose metadata is optional. */
        StoredApplicationSet: WithRequired<components["schemas"]["ApplicationSet"], "metadata">;
        /** @description Cluster as returned by the server (a stored object, or one the server rendered): metadata is always present. Request bodies use Cluster, whose metadata is optional. */
        StoredCluster: WithRequired<components["schemas"]["Cluster"], "metadata">;
        /** @description Operation as returned by the server (a stored object, or one the server rendered): metadata is always present. Request bodies use Operation, whose metadata is optional. */
        StoredOperation: WithRequired<components["schemas"]["Operation"], "metadata">;
        /** @description Repository as returned by the server (a stored object, or one the server rendered): metadata is always present. Request bodies use Repository, whose metadata is optional. */
        StoredRepository: WithRequired<components["schemas"]["Repository"], "metadata">;
        /** @description RepositoryCredential as returned by the server (a stored object, or one the server rendered): metadata is always present. Request bodies use RepositoryCredential, whose metadata is optional. */
        StoredRepositoryCredential: WithRequired<components["schemas"]["RepositoryCredential"], "metadata">;
        /** @description SCMProvider as returned by the server (a stored object, or one the server rendered): metadata is always present. Request bodies use SCMProvider, whose metadata is optional. */
        StoredSCMProvider: WithRequired<components["schemas"]["SCMProvider"], "metadata">;
        /** @description TerminalMessage is one WebSocket text frame of the web terminal: stdin and resize from the client, stdout from the server. */
        TerminalMessage: {
            /** @description Cols is the terminal width in characters (resize). */
            cols?: number;
            /** @description Data is the input (stdin) or output (stdout); resize frames omit it. */
            data?: string;
            /**
             * @description Operation is stdin or resize (client) or stdout (server); any other value ends the session.
             * @enum {string}
             */
            operation: "stdin" | "stdout" | "resize";
            /** @description Rows is the terminal height in characters (resize). */
            rows?: number;
        };
        /** @description TokenResponse carries a newly created API token. */
        TokenResponse: {
            token: string;
        };
        /** @description UpdatePasswordRequest is the body of PUT /api/v1/account/password. */
        UpdatePasswordRequest: {
            currentPassword?: string;
            /** @description Name is the account to update; empty means the caller. */
            name?: string;
            newPassword: string;
        };
        /** @description UserInfo describes the caller (GET /api/v1/session/userinfo). */
        UserInfo: {
            /** @description Groups are the values of the claims configured in patrol-rbac-cm "scopes". */
            groups?: string[];
            /** @description Iss is the issuer of the caller's token: "patrol" for every Patrol-issued session (local and SSO logins, API tokens), the identity provider's issuer for IdP tokens presented directly. */
            iss?: string;
            loggedIn: boolean;
            /** @description Username is the account name for local sessions and the email (or user id) for SSO sessions. */
            username?: string;
        };
        /** @description Version is the response of GET /api/v1/version. Unauthenticated callers (when anonymous access is disabled) only receive Version. */
        Version: {
            buildDate?: string;
            compiler?: string;
            extraBuildInfo?: {
                [key: string]: string;
            };
            gitCommit?: string;
            gitTag?: string;
            gitTreeState?: string;
            goVersion?: string;
            helmVersion?: string;
            jsonnetVersion?: string;
            kubectlVersion?: string;
            kustomizeVersion?: string;
            platform?: string;
            version: string;
        };
        WatchEvent_Application: {
            object: components["schemas"]["StoredApplication"];
            /** @enum {string} */
            type: "ADDED" | "MODIFIED" | "DELETED";
        };
        WatchEvent_ApplicationTree: {
            object: components["schemas"]["ApplicationTree"];
            /** @enum {string} */
            type: "ADDED" | "MODIFIED" | "DELETED";
        };
        ApplicationSetSpecGeneratorsItemMatrixGeneratorsItemPlugin: {
            configMapRef: {
                name: string;
            };
            input?: {
                parameters?: {
                    [key: string]: unknown;
                };
            };
            /** Format: int64 */
            requeueAfterSeconds?: number;
            template?: components["schemas"]["ApplicationSetSpecGeneratorsItemListTemplate"];
            values?: {
                [key: string]: string;
            };
        };
        /** @description JsonnetVar is a Jsonnet external variable or top-level argument. */
        ApplicationSpecSourceDirectoryJsonnetExtVarsItem: {
            /** @description Code evaluates Value as Jsonnet code instead of a string. */
            code?: boolean;
            name: string;
            value: string;
        };
        ApplicationSetSpecGeneratorsItemScmProviderBitbucketServerBasicAuth: {
            passwordRef: components["schemas"]["ApplicationSetSpecGeneratorsItemScmProviderGithubTokenRef"];
            username: string;
        };
        ApplicationSetSpecGeneratorsItemScmProviderBitbucketServerBearerToken: {
            tokenRef: components["schemas"]["ApplicationSetSpecGeneratorsItemScmProviderGithubTokenRef"];
        };
        ApplicationSetSpecGeneratorsItemScmProviderGithubProviderRef: {
            name: string;
        };
        ApplicationSetSpecGeneratorsItemScmProviderGithubTokenRef: {
            key: string;
            secretName: string;
        };
        ApplicationSetSpecGeneratorsItemScmProviderGitlabCaRef: {
            configMapName: string;
            key: string;
        };
        ApplicationSetSpecGeneratorsItemMatrixGeneratorsItem: {
            clusterDecisionResource?: components["schemas"]["ApplicationSetSpecGeneratorsItemClusterDecisionResource"];
            clusters?: components["schemas"]["ApplicationSetSpecGeneratorsItemClusters"];
            git?: components["schemas"]["ApplicationSetSpecGeneratorsItemGit"];
            list?: components["schemas"]["ApplicationSetSpecGeneratorsItemList"];
            matrix?: unknown;
            merge?: unknown;
            plugin?: components["schemas"]["ApplicationSetSpecGeneratorsItemMatrixGeneratorsItemPlugin"];
            pullRequest?: components["schemas"]["ApplicationSetSpecGeneratorsItemPullRequest"];
            scmProvider?: components["schemas"]["ApplicationSetSpecGeneratorsItemScmProvider"];
            selector?: {
                matchExpressions?: {
                    key: string;
                    operator: string;
                    values?: string[];
                }[];
                matchLabels?: {
                    [key: string]: string;
                };
            };
        };
        /** @description HelmFileParameter is a Helm --set-file parameter. */
        ApplicationSpecSourceHelmFileParametersItem: {
            /** @description Name of the parameter. */
            name?: string;
            /** @description Path of the file holding the value, relative to the source path. */
            path?: string;
        };
        /** @description HelmParameter is a Helm --set parameter. */
        ApplicationSpecSourceHelmParametersItem: {
            /** @description ForceString passes the value with --set-string. */
            forceString?: boolean;
            /** @description Name of the parameter. */
            name?: string;
            /** @description Value of the parameter. */
            value?: string;
        };
        ApplicationSetSpecGeneratorsItemListTemplate: {
            metadata: {
                annotations?: {
                    [key: string]: string;
                };
                finalizers?: string[];
                labels?: {
                    [key: string]: string;
                };
                name?: string;
                namespace?: string;
            };
            spec: {
                destination: {
                    name?: string;
                    namespace?: string;
                    server?: string;
                };
                ignoreDifferences?: {
                    group?: string;
                    jqPathExpressions?: string[];
                    jsonPointers?: string[];
                    kind: string;
                    managedFieldsManagers?: string[];
                    name?: string;
                    namespace?: string;
                }[];
                info?: {
                    name: string;
                    value: string;
                }[];
                project: string;
                /** Format: int64 */
                revisionHistoryLimit?: number;
                source?: {
                    chart?: string;
                    directory?: {
                        exclude?: string;
                        include?: string;
                        jsonnet?: {
                            extVars?: {
                                code?: boolean;
                                name: string;
                                value: string;
                            }[];
                            libs?: string[];
                            tlas?: {
                                code?: boolean;
                                name: string;
                                value: string;
                            }[];
                        };
                        recurse?: boolean;
                    };
                    helm?: {
                        apiVersions?: string[];
                        fileParameters?: {
                            name?: string;
                            path?: string;
                        }[];
                        ignoreMissingValueFiles?: boolean;
                        kubeVersion?: string;
                        namespace?: string;
                        parameters?: {
                            forceString?: boolean;
                            name?: string;
                            value?: string;
                        }[];
                        passCredentials?: boolean;
                        releaseName?: string;
                        skipCrds?: boolean;
                        skipSchemaValidation?: boolean;
                        skipTests?: boolean;
                        valueFiles?: string[];
                        values?: string;
                        valuesObject?: {
                            [key: string]: unknown;
                        };
                        version?: string;
                    };
                    kustomize?: {
                        apiVersions?: string[];
                        commonAnnotations?: {
                            [key: string]: string;
                        };
                        commonAnnotationsEnvsubst?: boolean;
                        commonLabels?: {
                            [key: string]: string;
                        };
                        components?: string[];
                        forceCommonAnnotations?: boolean;
                        forceCommonLabels?: boolean;
                        ignoreMissingComponents?: boolean;
                        images?: string[];
                        kubeVersion?: string;
                        labelIncludeTemplates?: boolean;
                        labelWithoutSelector?: boolean;
                        namePrefix?: string;
                        nameSuffix?: string;
                        namespace?: string;
                        patches?: {
                            options?: {
                                [key: string]: boolean;
                            };
                            patch?: string;
                            path?: string;
                            target?: {
                                annotationSelector?: string;
                                group?: string;
                                kind?: string;
                                labelSelector?: string;
                                name?: string;
                                namespace?: string;
                                version?: string;
                            };
                        }[];
                        replicas?: {
                            count: number | string;
                            name: string;
                        }[];
                        version?: string;
                    };
                    name?: string;
                    path?: string;
                    plugin?: {
                        env?: {
                            name: string;
                            value: string;
                        }[];
                        name?: string;
                        parameters?: {
                            array?: string[];
                            map?: {
                                [key: string]: string;
                            };
                            name?: string;
                            string?: string;
                        }[];
                    };
                    ref?: string;
                    repoURL: string;
                    tagPrefix?: string;
                    targetRevision?: string;
                };
                sourceHydrator?: {
                    drySource: {
                        directory?: {
                            exclude?: string;
                            include?: string;
                            jsonnet?: {
                                extVars?: {
                                    code?: boolean;
                                    name: string;
                                    value: string;
                                }[];
                                libs?: string[];
                                tlas?: {
                                    code?: boolean;
                                    name: string;
                                    value: string;
                                }[];
                            };
                            recurse?: boolean;
                        };
                        helm?: {
                            apiVersions?: string[];
                            fileParameters?: {
                                name?: string;
                                path?: string;
                            }[];
                            ignoreMissingValueFiles?: boolean;
                            kubeVersion?: string;
                            namespace?: string;
                            parameters?: {
                                forceString?: boolean;
                                name?: string;
                                value?: string;
                            }[];
                            passCredentials?: boolean;
                            releaseName?: string;
                            skipCrds?: boolean;
                            skipSchemaValidation?: boolean;
                            skipTests?: boolean;
                            valueFiles?: string[];
                            values?: string;
                            valuesObject?: {
                                [key: string]: unknown;
                            };
                            version?: string;
                        };
                        kustomize?: {
                            apiVersions?: string[];
                            commonAnnotations?: {
                                [key: string]: string;
                            };
                            commonAnnotationsEnvsubst?: boolean;
                            commonLabels?: {
                                [key: string]: string;
                            };
                            components?: string[];
                            forceCommonAnnotations?: boolean;
                            forceCommonLabels?: boolean;
                            ignoreMissingComponents?: boolean;
                            images?: string[];
                            kubeVersion?: string;
                            labelIncludeTemplates?: boolean;
                            labelWithoutSelector?: boolean;
                            namePrefix?: string;
                            nameSuffix?: string;
                            namespace?: string;
                            patches?: {
                                options?: {
                                    [key: string]: boolean;
                                };
                                patch?: string;
                                path?: string;
                                target?: {
                                    annotationSelector?: string;
                                    group?: string;
                                    kind?: string;
                                    labelSelector?: string;
                                    name?: string;
                                    namespace?: string;
                                    version?: string;
                                };
                            }[];
                            replicas?: {
                                count: number | string;
                                name: string;
                            }[];
                            version?: string;
                        };
                        path: string;
                        plugin?: {
                            env?: {
                                name: string;
                                value: string;
                            }[];
                            name?: string;
                            parameters?: {
                                array?: string[];
                                map?: {
                                    [key: string]: string;
                                };
                                name?: string;
                                string?: string;
                            }[];
                        };
                        repoURL: string;
                        targetRevision: string;
                    };
                    hydrateTo?: {
                        targetBranch: string;
                    };
                    syncSource: {
                        path: string;
                        repoURL?: string;
                        targetBranch: string;
                    };
                };
                sources?: {
                    chart?: string;
                    directory?: {
                        exclude?: string;
                        include?: string;
                        jsonnet?: {
                            extVars?: {
                                code?: boolean;
                                name: string;
                                value: string;
                            }[];
                            libs?: string[];
                            tlas?: {
                                code?: boolean;
                                name: string;
                                value: string;
                            }[];
                        };
                        recurse?: boolean;
                    };
                    helm?: {
                        apiVersions?: string[];
                        fileParameters?: {
                            name?: string;
                            path?: string;
                        }[];
                        ignoreMissingValueFiles?: boolean;
                        kubeVersion?: string;
                        namespace?: string;
                        parameters?: {
                            forceString?: boolean;
                            name?: string;
                            value?: string;
                        }[];
                        passCredentials?: boolean;
                        releaseName?: string;
                        skipCrds?: boolean;
                        skipSchemaValidation?: boolean;
                        skipTests?: boolean;
                        valueFiles?: string[];
                        values?: string;
                        valuesObject?: {
                            [key: string]: unknown;
                        };
                        version?: string;
                    };
                    kustomize?: {
                        apiVersions?: string[];
                        commonAnnotations?: {
                            [key: string]: string;
                        };
                        commonAnnotationsEnvsubst?: boolean;
                        commonLabels?: {
                            [key: string]: string;
                        };
                        components?: string[];
                        forceCommonAnnotations?: boolean;
                        forceCommonLabels?: boolean;
                        ignoreMissingComponents?: boolean;
                        images?: string[];
                        kubeVersion?: string;
                        labelIncludeTemplates?: boolean;
                        labelWithoutSelector?: boolean;
                        namePrefix?: string;
                        nameSuffix?: string;
                        namespace?: string;
                        patches?: {
                            options?: {
                                [key: string]: boolean;
                            };
                            patch?: string;
                            path?: string;
                            target?: {
                                annotationSelector?: string;
                                group?: string;
                                kind?: string;
                                labelSelector?: string;
                                name?: string;
                                namespace?: string;
                                version?: string;
                            };
                        }[];
                        replicas?: {
                            count: number | string;
                            name: string;
                        }[];
                        version?: string;
                    };
                    name?: string;
                    path?: string;
                    plugin?: {
                        env?: {
                            name: string;
                            value: string;
                        }[];
                        name?: string;
                        parameters?: {
                            array?: string[];
                            map?: {
                                [key: string]: string;
                            };
                            name?: string;
                            string?: string;
                        }[];
                    };
                    ref?: string;
                    repoURL: string;
                    tagPrefix?: string;
                    targetRevision?: string;
                }[];
                syncPolicy?: {
                    automated?: {
                        allowEmpty?: boolean;
                        enabled?: boolean;
                        prune?: boolean;
                        selfHeal?: boolean;
                    };
                    managedNamespaceMetadata?: components["schemas"]["ApplicationSpecSyncPolicyManagedNamespaceMetadata"];
                    retry?: {
                        backoff?: {
                            duration?: string;
                            /** Format: int64 */
                            factor?: number;
                            maxDuration?: string;
                        };
                        /** Format: int64 */
                        limit?: number;
                        refresh?: boolean;
                    };
                    syncOptions?: string[];
                };
            };
        };
        /** @description JWTToken records a token issued for a project role. */
        AppProjectSpecRolesItemJwtTokensItem: {
            /**
             * Format: int64
             * @description ExpiresAt is the token's exp claim (Unix seconds); 0 means no expiry.
             */
            exp?: number;
            /**
             * Format: int64
             * @description IssuedAt is the token's iat claim (Unix seconds).
             */
            iat: number;
            /** @description ID is the token's jti claim. */
            id?: string;
        };
        /** @description InitiatedBy records who started the sync. */
        ApplicationStatusHistoryItemInitiatedBy: {
            /** @description Automated is true if the controller requested the operation (automated sync or self-heal). */
            automated?: boolean;
            /** @description Username of the user who requested the operation. */
            username?: string;
        };
        /** @description Health of the resource. */
        ApplicationStatusResourcesItemHealth: {
            /** @description Message is a human-readable explanation of the health status. */
            message?: string;
            /** @description Status is the health status code. */
            status?: string;
        };
        ApplicationSetSpecGeneratorsItemClusterDecisionResource: {
            configMapRef: string;
            labelSelector?: {
                matchExpressions?: {
                    key: string;
                    operator: string;
                    values?: string[];
                }[];
                matchLabels?: {
                    [key: string]: string;
                };
            };
            name?: string;
            /** Format: int64 */
            requeueAfterSeconds?: number;
            template?: components["schemas"]["ApplicationSetSpecGeneratorsItemListTemplate"];
            values?: {
                [key: string]: string;
            };
        };
        ApplicationSetSpecGeneratorsItemClusters: {
            flatList?: boolean;
            selector?: {
                matchExpressions?: {
                    key: string;
                    operator: string;
                    values?: string[];
                }[];
                matchLabels?: {
                    [key: string]: string;
                };
            };
            template?: components["schemas"]["ApplicationSetSpecGeneratorsItemListTemplate"];
            values?: {
                [key: string]: string;
            };
        };
        ApplicationSetSpecGeneratorsItemGit: {
            directories?: {
                exclude?: boolean;
                path: string;
            }[];
            files?: {
                exclude?: boolean;
                path: string;
            }[];
            pathParamPrefix?: string;
            repoURL: string;
            /** Format: int64 */
            requeueAfterSeconds?: number;
            revision: string;
            template?: components["schemas"]["ApplicationSetSpecGeneratorsItemListTemplate"];
            values?: {
                [key: string]: string;
            };
        };
        ApplicationSetSpecGeneratorsItemList: {
            elements?: unknown[];
            elementsYaml?: string;
            template?: components["schemas"]["ApplicationSetSpecGeneratorsItemListTemplate"];
        };
        ApplicationSetSpecGeneratorsItemPullRequest: {
            azuredevops?: {
                api?: string;
                labels?: string[];
                organization: string;
                project: string;
                providerRef?: components["schemas"]["ApplicationSetSpecGeneratorsItemScmProviderGithubProviderRef"];
                repo: string;
                tokenRef?: components["schemas"]["ApplicationSetSpecGeneratorsItemScmProviderGithubTokenRef"];
            };
            bitbucket?: {
                api?: string;
                basicAuth?: components["schemas"]["ApplicationSetSpecGeneratorsItemScmProviderBitbucketServerBasicAuth"];
                bearerToken?: {
                    tokenRef: components["schemas"]["ApplicationSetSpecGeneratorsItemScmProviderGithubTokenRef"];
                };
                owner: string;
                providerRef?: components["schemas"]["ApplicationSetSpecGeneratorsItemScmProviderGithubProviderRef"];
                repo: string;
            };
            bitbucketServer?: {
                api: string;
                basicAuth?: components["schemas"]["ApplicationSetSpecGeneratorsItemScmProviderBitbucketServerBasicAuth"];
                bearerToken?: components["schemas"]["ApplicationSetSpecGeneratorsItemScmProviderBitbucketServerBearerToken"];
                caRef?: components["schemas"]["ApplicationSetSpecGeneratorsItemScmProviderGitlabCaRef"];
                insecure?: boolean;
                project: string;
                providerRef?: components["schemas"]["ApplicationSetSpecGeneratorsItemScmProviderGithubProviderRef"];
                repo: string;
            };
            continueOnRepoNotFoundError?: boolean;
            filters?: {
                branchMatch?: string;
                targetBranchMatch?: string;
                titleMatch?: string;
            }[];
            gitea?: {
                api: string;
                insecure?: boolean;
                labels?: string[];
                owner: string;
                providerRef?: components["schemas"]["ApplicationSetSpecGeneratorsItemScmProviderGithubProviderRef"];
                repo: string;
                tokenRef?: components["schemas"]["ApplicationSetSpecGeneratorsItemScmProviderGithubTokenRef"];
            };
            github?: {
                api?: string;
                appSecretName?: string;
                labels?: string[];
                owner: string;
                providerRef?: components["schemas"]["ApplicationSetSpecGeneratorsItemScmProviderGithubProviderRef"];
                repo: string;
                tokenRef?: components["schemas"]["ApplicationSetSpecGeneratorsItemScmProviderGithubTokenRef"];
            };
            gitlab?: {
                api?: string;
                caRef?: components["schemas"]["ApplicationSetSpecGeneratorsItemScmProviderGitlabCaRef"];
                insecure?: boolean;
                labels?: string[];
                project: string;
                providerRef?: components["schemas"]["ApplicationSetSpecGeneratorsItemScmProviderGithubProviderRef"];
                pullRequestState?: string;
                tokenRef?: components["schemas"]["ApplicationSetSpecGeneratorsItemScmProviderGithubTokenRef"];
            };
            /** Format: int64 */
            requeueAfterSeconds?: number;
            template?: components["schemas"]["ApplicationSetSpecGeneratorsItemListTemplate"];
            values?: {
                [key: string]: string;
            };
        };
        ApplicationSetSpecGeneratorsItemScmProvider: {
            awsCodeCommit?: {
                allBranches?: boolean;
                providerRef?: components["schemas"]["ApplicationSetSpecGeneratorsItemScmProviderGithubProviderRef"];
                region?: string;
                role?: string;
                tagFilters?: {
                    key: string;
                    value?: string;
                }[];
            };
            azureDevOps?: {
                accessTokenRef?: components["schemas"]["ApplicationSetSpecGeneratorsItemScmProviderGithubTokenRef"];
                allBranches?: boolean;
                api?: string;
                organization: string;
                providerRef?: components["schemas"]["ApplicationSetSpecGeneratorsItemScmProviderGithubProviderRef"];
                teamProject: string;
            };
            bitbucket?: {
                allBranches?: boolean;
                appPasswordRef?: components["schemas"]["ApplicationSetSpecGeneratorsItemScmProviderGithubTokenRef"];
                owner: string;
                providerRef?: components["schemas"]["ApplicationSetSpecGeneratorsItemScmProviderGithubProviderRef"];
                user?: string;
            };
            bitbucketServer?: {
                allBranches?: boolean;
                api: string;
                basicAuth?: components["schemas"]["ApplicationSetSpecGeneratorsItemScmProviderBitbucketServerBasicAuth"];
                bearerToken?: components["schemas"]["ApplicationSetSpecGeneratorsItemScmProviderBitbucketServerBearerToken"];
                caRef?: components["schemas"]["ApplicationSetSpecGeneratorsItemScmProviderGitlabCaRef"];
                insecure?: boolean;
                project: string;
                providerRef?: components["schemas"]["ApplicationSetSpecGeneratorsItemScmProviderGithubProviderRef"];
            };
            cloneProtocol?: string;
            filters?: {
                branchMatch?: string;
                labelMatch?: string;
                pathsDoNotExist?: string[];
                pathsExist?: string[];
                repositoryMatch?: string;
            }[];
            gitea?: {
                allBranches?: boolean;
                api: string;
                excludeArchivedRepos?: boolean;
                insecure?: boolean;
                owner: string;
                providerRef?: components["schemas"]["ApplicationSetSpecGeneratorsItemScmProviderGithubProviderRef"];
                tokenRef?: components["schemas"]["ApplicationSetSpecGeneratorsItemScmProviderGithubTokenRef"];
            };
            github?: {
                allBranches?: boolean;
                api?: string;
                appSecretName?: string;
                excludeArchivedRepos?: boolean;
                organization: string;
                providerRef?: components["schemas"]["ApplicationSetSpecGeneratorsItemScmProviderGithubProviderRef"];
                tokenRef?: components["schemas"]["ApplicationSetSpecGeneratorsItemScmProviderGithubTokenRef"];
            };
            gitlab?: {
                allBranches?: boolean;
                api?: string;
                caRef?: components["schemas"]["ApplicationSetSpecGeneratorsItemScmProviderGitlabCaRef"];
                group: string;
                includeArchivedRepos?: boolean;
                includeSharedProjects?: boolean;
                includeSubgroups?: boolean;
                insecure?: boolean;
                providerRef?: components["schemas"]["ApplicationSetSpecGeneratorsItemScmProviderGithubProviderRef"];
                tokenRef?: components["schemas"]["ApplicationSetSpecGeneratorsItemScmProviderGithubTokenRef"];
                topic?: string;
            };
            /** Format: int64 */
            requeueAfterSeconds?: number;
            template?: components["schemas"]["ApplicationSetSpecGeneratorsItemListTemplate"];
            values?: {
                [key: string]: string;
            };
        };
        /** @description SyncOperationResource identifies a resource of a selective sync. "*" matches any value. */
        OperationSpecSyncResourcesItem: {
            group?: string;
            kind: string;
            name: string;
            namespace?: string;
        };
        /** @description Directory holds options for plain YAML/JSON or Jsonnet directories. */
        ApplicationSpecSourceDirectory: {
            /** @description Exclude is a glob of paths excluded from manifest generation. */
            exclude?: string;
            /** @description Include is a glob of paths included in manifest generation. */
            include?: string;
            /** @description Jsonnet holds Jsonnet-specific options. */
            jsonnet?: {
                /** @description ExtVars are Jsonnet external variables. */
                extVars?: components["schemas"]["ApplicationSpecSourceDirectoryJsonnetExtVarsItem"][];
                /** @description Libs are additional library search directories. */
                libs?: string[];
                /** @description TLAs are Jsonnet top-level arguments. */
                tlas?: components["schemas"]["ApplicationSpecSourceDirectoryJsonnetExtVarsItem"][];
            };
            /** @description Recurse scans subdirectories for manifests. */
            recurse?: boolean;
        };
        /** @description Helm holds Helm-specific options. */
        ApplicationSpecSourceHelm: {
            /**
             * @description APIVersions are the API versions passed to Helm ([group/]version/kind). Defaults to the
             *     destination's API versions.
             */
            apiVersions?: string[];
            /** @description FileParameters are passed to helm template as --set-file values. */
            fileParameters?: components["schemas"]["ApplicationSpecSourceHelmFileParametersItem"][];
            /** @description IgnoreMissingValueFiles skips value files that do not exist instead of failing. */
            ignoreMissingValueFiles?: boolean;
            /** @description KubeVersion is the Kubernetes version passed to Helm. Defaults to the destination's version. */
            kubeVersion?: string;
            /** @description Namespace is the namespace to template with. Defaults to the destination namespace. */
            namespace?: string;
            /** @description Parameters are passed to helm template as --set values. */
            parameters?: components["schemas"]["ApplicationSpecSourceHelmParametersItem"][];
            /** @description PassCredentials passes credentials to all domains (helm --pass-credentials). */
            passCredentials?: boolean;
            /** @description ReleaseName is the Helm release name. Defaults to the Application name. */
            releaseName?: string;
            /** @description SkipCrds skips installing CRDs from the chart (helm --skip-crds). */
            skipCrds?: boolean;
            /** @description SkipSchemaValidation skips JSON schema validation (helm --skip-schema-validation). */
            skipSchemaValidation?: boolean;
            /** @description SkipTests skips test manifests (helm --skip-tests). */
            skipTests?: boolean;
            /** @description ValueFiles is a list of Helm value files to use when generating a template. */
            valueFiles?: string[];
            /** @description Values is a YAML block of Helm values. ValuesObject takes precedence when both are set. */
            values?: string;
            /** @description ValuesObject holds Helm values as a structured object. Takes precedence over Values. */
            valuesObject?: {
                [key: string]: unknown;
            };
            /** @description Version is the Helm major version to use for templating ("3"). */
            version?: string;
        };
        /** @description Kustomize holds Kustomize-specific options. */
        ApplicationSpecSourceKustomize: {
            /** @description APIVersions are the API versions passed to Helm charts inflated by Kustomize. */
            apiVersions?: string[];
            /** @description CommonAnnotations are added to all rendered resources. */
            commonAnnotations?: {
                [key: string]: string;
            };
            /** @description CommonAnnotationsEnvsubst substitutes environment variables in CommonAnnotations values. */
            commonAnnotationsEnvsubst?: boolean;
            /** @description CommonLabels are added to all rendered resources. */
            commonLabels?: {
                [key: string]: string;
            };
            /** @description Components are Kustomize components added to the kustomization. */
            components?: string[];
            /** @description ForceCommonAnnotations overwrites existing annotations with CommonAnnotations. */
            forceCommonAnnotations?: boolean;
            /** @description ForceCommonLabels overwrites existing labels with CommonLabels. */
            forceCommonLabels?: boolean;
            /** @description IgnoreMissingComponents skips components that do not exist instead of failing. */
            ignoreMissingComponents?: boolean;
            /** @description Images are image overrides. */
            images?: string[];
            /** @description KubeVersion is the Kubernetes version passed to Helm charts inflated by Kustomize. */
            kubeVersion?: string;
            /** @description LabelIncludeTemplates applies CommonLabels to resource templates (requires LabelWithoutSelector). */
            labelIncludeTemplates?: boolean;
            /** @description LabelWithoutSelector applies CommonLabels without adding them to selectors. */
            labelWithoutSelector?: boolean;
            /** @description NamePrefix overrides the kustomization namePrefix. */
            namePrefix?: string;
            /** @description NameSuffix overrides the kustomization nameSuffix. */
            nameSuffix?: string;
            /** @description Namespace sets the namespace of all rendered resources. */
            namespace?: string;
            /** @description Patches are Kustomize patches added to the kustomization. */
            patches?: {
                /** @description Options are Kustomize patch options (allowNameChange, allowKindChange). */
                options?: {
                    [key: string]: boolean;
                };
                /** @description Patch is an inline patch. */
                patch?: string;
                /** @description Path of a patch file, relative to the source path. */
                path?: string;
                /** @description Target selects the resources to patch. */
                target?: {
                    /** @description AnnotationSelector selects resources by annotation. */
                    annotationSelector?: string;
                    group?: string;
                    kind?: string;
                    /** @description LabelSelector selects resources by label. */
                    labelSelector?: string;
                    /** @description Name of the resource. */
                    name?: string;
                    /** @description Namespace of the resource. */
                    namespace?: string;
                    version?: string;
                };
            }[];
            /** @description Replicas are replica count overrides. */
            replicas?: {
                /** @description Count is the number of replicas. */
                count: number | string;
                /** @description Name of the Deployment or StatefulSet. */
                name: string;
            }[];
            /** @description Version selects a configured Kustomize version. */
            version?: string;
        };
        /** @description Plugin holds Config Management Plugin options. */
        ApplicationSpecSourcePlugin: {
            /** @description Env are environment variables passed to the plugin. */
            env?: {
                /** @description Name of the variable, usually upper case. */
                name: string;
                /** @description Value of the variable. */
                value: string;
            }[];
            /** @description Name of the plugin. If omitted the plugin is selected by discovery. */
            name?: string;
            /** @description Parameters are passed to the plugin. */
            parameters?: {
                /** @description Array is the value of an array parameter. */
                array?: string[];
                /** @description Map is the value of a map parameter. */
                map?: {
                    [key: string]: string;
                };
                /** @description Name of the parameter. */
                name?: string;
                /** @description String is the value of a string parameter. */
                string?: string;
            }[];
        };
        /** @description ManagedNamespaceMetadata is applied to the destination namespace when CreateNamespace=true. */
        ApplicationSpecSyncPolicyManagedNamespaceMetadata: {
            annotations?: {
                [key: string]: string;
            };
            labels?: {
                [key: string]: string;
            };
        };
        /** @description Retry controls retries of failed syncs. */
        ApplicationSpecSyncPolicyRetry: {
            /** @description Backoff controls the delay between retries. */
            backoff?: {
                /** @description Duration is the base delay, in seconds or as a Go duration ("5s", "2m"). Default "5s". */
                duration?: string;
                /**
                 * Format: int64
                 * @description Factor multiplies the delay after each failed attempt. Default 2.
                 */
                factor?: number;
                /** @description MaxDuration caps the delay, in seconds or as a Go duration. Default "3m". */
                maxDuration?: string;
            };
            /**
             * Format: int64
             * @description Limit is the maximum number of retries; 0 disables retries and a negative value retries forever.
             */
            limit?: number;
            /** @description Refresh retries with the latest revision instead of the originally resolved one. */
            refresh?: boolean;
        };
        /** @description AWSAuthConfig authenticates with AWS IAM (EKS). */
        ClusterSpecConfigAwsAuthConfig: {
            /** @description ClusterName is the EKS cluster name. */
            clusterName?: string;
            /** @description Profile is the AWS profile to use. */
            profile?: string;
            /** @description RoleARN is an IAM role to assume. */
            roleARN?: string;
        };
        /**
         * @description ExecProviderConfig authenticates with an exec credential plugin. Do not put credentials in its
         *     args or env; use CredentialsRef.
         */
        ClusterSpecConfigExecProviderConfig: {
            /** @description APIVersion is the preferred ExecCredential version. */
            apiVersion?: string;
            /** @description Args passed to the command. */
            args?: string[];
            /** @description Command to execute. */
            command?: string;
            /** @description Config is passed to the plugin as ExecCredential.spec.cluster.config. */
            config?: {
                [key: string]: unknown;
            };
            /** @description Env are additional environment variables for the command. */
            env?: {
                [key: string]: string;
            };
            /** @description InstallHint is shown when the command is not found. */
            installHint?: string;
            /** @description ProvideClusterInfo passes cluster information in KUBERNETES_EXEC_INFO. */
            provideClusterInfo?: boolean;
        };
        /** @description SyncStrategy selects apply or hook strategy (hook is the default). */
        OperationSpecSyncSyncStrategy: {
            /** @description Apply applies resources without running hooks. */
            apply?: {
                /**
                 * @description Force deletes and re-creates resources when a patch fails.
                 * @default false
                 */
                force?: boolean;
            };
            /** @description Hook applies resources and runs hooks. This is the default. */
            hook?: {
                /**
                 * @description Force deletes and re-creates resources when a patch fails.
                 * @default false
                 */
                force?: boolean;
            };
        };
        /** @description ResourceIgnoreDifferences selects resources and the fields of them ignored during comparison. */
        ApplicationSpecIgnoreDifferencesItem: {
            /** @description Group of the resources ("" for the core group). */
            group?: string;
            /** @description JQPathExpressions of ignored fields. */
            jqPathExpressions?: string[];
            /** @description JSONPointers (RFC 6901) of ignored fields. */
            jsonPointers?: string[];
            /** @description Kind of the resources. */
            kind: string;
            /** @description ManagedFieldsManagers are field managers whose changes are ignored. */
            managedFieldsManagers?: string[];
            /** @description Name of the resource; all resources of the kind if empty. */
            name?: string;
            /** @description Namespace of the resource; all namespaces if empty. */
            namespace?: string;
        };
        /** @description Info is a name/value pair of informational metadata. */
        ApplicationSpecInfoItem: {
            /** @description Name of the item. */
            name: string;
            /** @description Value of the item. */
            value: string;
        };
        /** @description ClusterResourceRestrictionItem selects cluster-scoped resources by group, kind and name glob. */
        AppProjectSpecClusterResourceWhitelistItem: {
            group: string;
            kind: string;
            /** @description Name is a glob of resource names; all names if empty. */
            name?: string;
        };
        /** @description SyncWindow allows or denies syncs during a recurring time window. */
        AppProjectSpecSyncWindowsItem: {
            /** @description UseAndOperator matches applications AND namespaces AND clusters instead of OR. */
            andOperator?: boolean;
            /** @description Applications are Application name globs the window applies to. */
            applications?: string[];
            /** @description Clusters are cluster name/server globs the window applies to. */
            clusters?: string[];
            /** @description Description of the window (e.g. a change ticket). */
            description?: string;
            /** @description Duration is how long the window stays open (Go duration, e.g. "1h"). */
            duration?: string;
            /**
             * @description Kind is "allow" or "deny".
             * @enum {string}
             */
            kind?: "allow" | "deny";
            /** @description ManualSync allows manual syncs while the window would block them. */
            manualSync?: boolean;
            /** @description Namespaces are namespace globs the window applies to. */
            namespaces?: string[];
            /** @description Schedule is a 5-field cron expression or a descriptor (@hourly, @daily, ...). */
            schedule?: string;
            /**
             * @description SyncOverrun lets syncs that started before a deny window (or inside an allow window) continue
             *     after the window boundary.
             */
            syncOverrun?: boolean;
            /** @description TimeZone of the schedule (IANA name). Defaults to UTC. */
            timeZone?: string;
        };
        /** @description Destination is the target cluster and namespace. */
        ApplicationSpecDestination: {
            /** @description Name is the name of the target Cluster. */
            name?: string;
            /** @description Namespace is the target namespace for namespaced resources without a namespace. */
            namespace?: string;
            /** @description Server is the URL of the target cluster's API server. */
            server?: string;
        };
        /**
         * @description Source is the single source of the Application. Mutually exclusive with Sources and
         *     SourceHydrator.
         */
        ApplicationSpecSource: {
            /** @description Chart is the Helm chart name; required for sources in a Helm repository. */
            chart?: string;
            directory?: components["schemas"]["ApplicationSpecSourceDirectory"];
            helm?: components["schemas"]["ApplicationSpecSourceHelm"];
            kustomize?: components["schemas"]["ApplicationSpecSourceKustomize"];
            /** @description Name identifies the source in the UI and CLI of multi-source Applications. */
            name?: string;
            /** @description Path is a directory path within the repository. Only valid for Git and OCI sources. */
            path?: string;
            plugin?: components["schemas"]["ApplicationSpecSourcePlugin"];
            /**
             * @description Ref names this source so other sources of a multi-source Application can reference its files
             *     as "$<ref>/<path>". Ignored for single-source Applications.
             */
            ref?: string;
            /** @description RepoURL is the URL of the repository (Git, Helm or OCI) that contains the application manifests. */
            repoURL: string;
            /**
             * @description TagPrefix restricts git tags to those with this prefix before targetRevision is evaluated as a
             *     semver constraint. The prefix is stripped before comparison and re-added to the resolved tag.
             */
            tagPrefix?: string;
            /**
             * @description TargetRevision is the revision to sync to: a commit, tag or branch for Git (HEAD if omitted), a
             *     semver constraint or version for Helm charts.
             */
            targetRevision?: string;
        };
        /**
         * @description SourceHydrator renders a dry source and commits the hydrated manifests to git before syncing
         *     them. Mutually exclusive with Source and Sources.
         */
        ApplicationSpecSourceHydrator: {
            /** @description DrySource is where the un-rendered manifests live. */
            drySource: {
                /** @description ApplicationSourceDirectory holds options for plain YAML/JSON or Jsonnet directories. */
                directory?: components["schemas"]["ApplicationSpecSourceDirectory"];
                /** @description ApplicationSourceHelm holds Helm-specific options. */
                helm?: components["schemas"]["ApplicationSpecSourceHelm"];
                /** @description ApplicationSourceKustomize holds Kustomize-specific options. */
                kustomize?: components["schemas"]["ApplicationSpecSourceKustomize"];
                /** @description Path is the directory of the dry manifests within the repository. */
                path: string;
                /** @description ApplicationSourcePlugin holds Config Management Plugin options. */
                plugin?: components["schemas"]["ApplicationSpecSourcePlugin"];
                /** @description RepoURL is the URL of the git repository holding the dry manifests. */
                repoURL: string;
                /** @description TargetRevision is the revision of the dry source to hydrate. */
                targetRevision: string;
            };
            /** @description HydrateTo is an optional staging branch; an external process moves manifests to SyncSource. */
            hydrateTo?: {
                /** @description TargetBranch is the branch hydrated manifests are committed to. */
                targetBranch: string;
            };
            /** @description SyncSource is where hydrated manifests are committed (unless HydrateTo is set) and synced from. */
            syncSource: {
                /**
                 * @description Path is the directory of hydrated manifests. It must not be the repository root: it must contain
                 *     a character other than "." and "/" (Argo CD's pattern ^.{2,}|[^./]$ also accepts "./", "//" and
                 *     "..").
                 */
                path: string;
                /** @description RepoURL is the repository of hydrated manifests. Defaults to DrySource.RepoURL. */
                repoURL?: string;
                /**
                 * @description TargetBranch is the branch hydrated manifests are synced from (and committed to, unless
                 *     HydrateTo is set).
                 */
                targetBranch: string;
            };
        };
        /**
         * @description AzureServicePrincipal authenticates with an Azure service principal; the client secret is read
         *     from the credentials Secret (key azureServicePrincipalClientSecret).
         */
        RepositorySpecAzureServicePrincipal: {
            /** @description ActiveDirectoryEndpoint (default https://login.microsoftonline.com). */
            activeDirectoryEndpoint?: string;
            /** @description ClientID of the service principal. */
            clientID: string;
            /** @description TenantID of the service principal. */
            tenantID: string;
        };
        /**
         * @description CredentialsRef names a Secret in the same namespace holding credentials. Recognized keys:
         *     username, password, bearerToken, sshPrivateKey, tlsClientCertData, tlsClientCertKey,
         *     githubAppPrivateKey, gcpServiceAccountKey, azureServicePrincipalClientSecret.
         */
        RepositorySpecCredentialsRef: {
            /** @description Name of the Secret. */
            name: string;
        };
        /**
         * @description GitHubApp authenticates as a GitHub App installation; the private key is read from the
         *     credentials Secret (key githubAppPrivateKey) unless SCMProviderRef supplies the App.
         */
        RepositorySpecGithubApp: {
            /**
             * Format: int64
             * @description AppID of the GitHub App.
             */
            appID: number;
            /** @description EnterpriseBaseURL is the GitHub Enterprise API URL (default https://api.github.com). */
            enterpriseBaseURL?: string;
            /**
             * Format: int64
             * @description InstallationID of the App installation. When unset, the installation is discovered from the
             *     organization (owner) in the repository URL, which lets one RepositoryCredential serve every
             *     organization the App is installed in.
             */
            installationID?: number;
        };
        /** @description ConnectionState is the result of the last connection check. */
        RepositoryStatusConnectionState: {
            /**
             * Format: date-time
             * @description AttemptedAt is when the connection status was determined. On a Cluster object it moves only
             *     when the status or message changes, so it says since when that state holds.
             */
            attemptedAt?: string;
            /** @description Message is a human-readable description of the connection status. */
            message?: string;
            /**
             * @description Status of the last connection attempt.
             * @enum {string}
             */
            status: "Successful" | "Failed" | "Unknown";
        };
        /** @description CASecretRef references a PEM CA bundle trusted for the API. */
        SCMProviderSpecCaSecretRef: {
            /** @description Key within the Secret's data. */
            key: string;
            /** @description Name of the Secret. */
            name: string;
        };
        /** @description Spec is the desired state of the Application. */
        ApplicationSpec: {
            destination: components["schemas"]["ApplicationSpecDestination"];
            /** @description IgnoreDifferences lists resource fields ignored during comparison. */
            ignoreDifferences?: components["schemas"]["ApplicationSpecIgnoreDifferencesItem"][];
            /** @description Info holds informational name/value pairs (URLs, contacts, notes) shown in the UI. */
            info?: components["schemas"]["ApplicationSpecInfoItem"][];
            /** @description Project is the AppProject the Application belongs to. The empty string means "default". */
            project: string;
            /**
             * Format: int64
             * @description RevisionHistoryLimit is the number of entries kept in status.history and the number of terminal
             *     Operations retained for the Application. Defaults to 10 when unset.
             */
            revisionHistoryLimit?: number;
            source?: components["schemas"]["ApplicationSpecSource"];
            sourceHydrator?: components["schemas"]["ApplicationSpecSourceHydrator"];
            /**
             * @description Sources are the sources of a multi-source Application. Mutually exclusive with Source and
             *     SourceHydrator.
             */
            sources?: components["schemas"]["ApplicationSpecSource"][];
            /** @description SyncPolicy controls when and how syncs are performed. */
            syncPolicy?: {
                /** @description Automated enables automated sync. */
                automated?: {
                    /** @description AllowEmpty allows automated sync to prune all resources (default false). */
                    allowEmpty?: boolean;
                    /** @description Enabled turns automated sync on or off while keeping its settings (default true). */
                    enabled?: boolean;
                    /** @description Prune deletes resources that are no longer desired during automated sync (default false). */
                    prune?: boolean;
                    /** @description SelfHeal re-syncs when live state drifts from desired state (default false). */
                    selfHeal?: boolean;
                };
                managedNamespaceMetadata?: components["schemas"]["ApplicationSpecSyncPolicyManagedNamespaceMetadata"];
                retry?: components["schemas"]["ApplicationSpecSyncPolicyRetry"];
                /** @description SyncOptions are Application-wide sync options ("Key=Value"). */
                syncOptions?: string[];
            };
        };
        /** @description Spec is the cluster configuration. */
        ClusterSpec: {
            /** @description ClusterResources allows cluster-scoped resources when Namespaces is set. */
            clusterResources?: boolean;
            /** @description Config holds connection settings. */
            config?: {
                awsAuthConfig?: components["schemas"]["ClusterSpecConfigAwsAuthConfig"];
                /**
                 * @description CredentialsRef names a Secret in the same namespace holding credentials and CA data. Recognized
                 *     keys: bearerToken, username, password, caData, certData, keyData.
                 */
                credentialsRef?: components["schemas"]["RepositorySpecCredentialsRef"];
                /** @description DisableCompression disables response compression. */
                disableCompression?: boolean;
                execProviderConfig?: components["schemas"]["ClusterSpecConfigExecProviderConfig"];
                /** @description ProxyURL is the proxy used for all requests to the cluster. */
                proxyURL?: string;
                /** @description TLSClientConfig holds TLS settings. */
                tlsClientConfig?: {
                    /** @description Insecure skips server certificate verification. For testing only. */
                    insecure?: boolean;
                    /** @description ServerName is used for SNI and certificate verification instead of the server host name. */
                    serverName?: string;
                };
            };
            /** @description Name of the cluster, referenced by Application destinations (spec.destination.name). */
            name: string;
            /**
             * @description Namespaces restricts management to these namespaces. When set, cluster-scoped resources are
             *     ignored unless ClusterResources is true.
             */
            namespaces?: string[];
            /** @description Project scopes the cluster to an AppProject. */
            project?: string;
            /**
             * @description Server is the URL of the cluster's API server. It cannot change: the status describes this
             *     destination, so another one needs a new Cluster.
             */
            server: string;
            /**
             * Format: int64
             * @description Shard pins the cluster to a controller shard. Computed by the sharding algorithm when unset.
             */
            shard?: number;
        };
        /** @description Status is the observed state of the cluster. */
        ClusterStatus: {
            /**
             * Format: int64
             * @description AssignedShard is the controller shard managing the cluster.
             */
            assignedShard?: number;
            /** @description Conditions are the current conditions (Ready, ConnectionFailed). */
            conditions?: {
                /**
                 * Format: date-time
                 * @description lastTransitionTime is the last time the condition transitioned from one status to another.
                 *     This should be when the underlying condition changed.  If that is not known, then using the time when the API field changed is acceptable.
                 */
                lastTransitionTime: string;
                /**
                 * @description message is a human readable message indicating details about the transition.
                 *     This may be an empty string.
                 */
                message: string;
                /**
                 * Format: int64
                 * @description observedGeneration represents the .metadata.generation that the condition was set based upon.
                 *     For instance, if .metadata.generation is currently 12, but the .status.conditions[x].observedGeneration is 9, the condition is out of date
                 *     with respect to the current state of the instance.
                 */
                observedGeneration?: number;
                /**
                 * @description reason contains a programmatic identifier indicating the reason for the condition's last transition.
                 *     Producers of specific condition types may define expected values and meanings for this field,
                 *     and whether the values are considered a guaranteed API.
                 *     The value should be a CamelCase string.
                 *     This field may not be empty.
                 */
                reason: string;
                /**
                 * @description status of the condition, one of True, False, Unknown.
                 * @enum {string}
                 */
                status: "True" | "False" | "Unknown";
                /** @description type of condition in CamelCase or in foo.example.com/CamelCase. */
                type: string;
            }[];
            /** @description ConnectionState is the result of the last connection check. */
            connectionState?: components["schemas"]["RepositoryStatusConnectionState"];
            /** @description Info holds cache and usage information. */
            info?: {
                /** @description APIVersions are the API versions served by the cluster. */
                apiVersions?: string[];
                /**
                 * Format: int64
                 * @description ApplicationsCount is the number of Applications targeting the cluster.
                 */
                applicationsCount?: number;
                /** @description CacheInfo describes the controller's live-state cache of the cluster. */
                cacheInfo?: {
                    /**
                     * Format: int64
                     * @description APIsCount is the number of watched APIs.
                     */
                    apisCount?: number;
                    /**
                     * Format: date-time
                     * @description LastCacheSyncTime is when the cache was last fully synchronized.
                     */
                    lastCacheSyncTime?: string;
                    /**
                     * Format: int64
                     * @description ResourcesCount is the number of cached resources.
                     */
                    resourcesCount?: number;
                };
            };
            /**
             * Format: int64
             * @description ObservedGeneration is the metadata.generation the status reflects.
             */
            observedGeneration?: number;
            /** @description ServerVersion is the Kubernetes version of the cluster. */
            serverVersion?: string;
        };
        /** @description Spec is the repository configuration. */
        RepositorySpec: {
            azureServicePrincipal?: components["schemas"]["RepositorySpecAzureServicePrincipal"];
            credentialsRef?: components["schemas"]["RepositorySpecCredentialsRef"];
            /**
             * Format: int64
             * @description Depth is the clone depth for shallow clones; 0 means a full clone.
             */
            depth?: number;
            /** @description EnableLFS fetches git-lfs objects (git only). */
            enableLFS?: boolean;
            /** @description EnableOCI enables Helm OCI support for a Helm repository. */
            enableOCI?: boolean;
            /** @description ForceHTTPBasicAuth sends basic auth on the first request instead of waiting for a challenge. */
            forceHTTPBasicAuth?: boolean;
            githubApp?: components["schemas"]["RepositorySpecGithubApp"];
            /** @description Insecure skips TLS certificate and SSH host key verification. */
            insecure?: boolean;
            /**
             * @description InsecureIgnoreHostKey skips SSH host key verification (git only).
             *
             *     Deprecated: use Insecure.
             */
            insecureIgnoreHostKey?: boolean;
            /** @description InsecureOCIForceHTTP connects to an OCI registry over plain HTTP (OCI only). */
            insecureOCIForceHTTP?: boolean;
            /** @description Name is the Helm repository name used in dependency resolution (Helm repositories only). */
            name?: string;
            /** @description NoProxy is a comma-separated list of hosts that bypass Proxy. */
            noProxy?: string;
            /**
             * @description Project scopes the repository to an AppProject; it is then implicitly allowed in that project's
             *     sourceRepos and usable only by its Applications.
             */
            project?: string;
            /** @description Proxy is the HTTP(S) proxy URL used to reach the repository. */
            proxy?: string;
            /**
             * @description Purpose of the credentials.
             * @default read
             * @enum {string}
             */
            purpose?: "read" | "write";
            /**
             * @description SCMProviderRef obtains credentials (e.g. GitHub App installation tokens) from an SCMProvider in
             *     the same namespace.
             */
            scmProviderRef?: {
                /** @description Name of the SCMProvider. */
                name: string;
            };
            /**
             * @description Type of the repository.
             * @default git
             * @enum {string}
             */
            type?: "git" | "helm" | "oci";
            /** @description URL of the repository (https://, ssh://, git@host:path, oci://, or a Helm repository URL). */
            url: string;
            /** @description UseAzureWorkloadIdentity authenticates with Azure Workload Identity. */
            useAzureWorkloadIdentity?: boolean;
            /**
             * @description WebhookManifestCacheWarmDisabled skips pre-rendering (manifest cache warming) of affected
             *     Applications when a webhook for this repository arrives; they are refreshed instead. Useful for
             *     large monorepos.
             */
            webhookManifestCacheWarmDisabled?: boolean;
        };
        /** @description Status is the observed state of the repository. */
        RepositoryStatus: {
            /** @description Conditions are the current conditions (Ready, ConnectionFailed). */
            conditions?: {
                /**
                 * Format: date-time
                 * @description lastTransitionTime is the last time the condition transitioned from one status to another.
                 *     This should be when the underlying condition changed.  If that is not known, then using the time when the API field changed is acceptable.
                 */
                lastTransitionTime: string;
                /**
                 * @description message is a human readable message indicating details about the transition.
                 *     This may be an empty string.
                 */
                message: string;
                /**
                 * Format: int64
                 * @description observedGeneration represents the .metadata.generation that the condition was set based upon.
                 *     For instance, if .metadata.generation is currently 12, but the .status.conditions[x].observedGeneration is 9, the condition is out of date
                 *     with respect to the current state of the instance.
                 */
                observedGeneration?: number;
                /**
                 * @description reason contains a programmatic identifier indicating the reason for the condition's last transition.
                 *     Producers of specific condition types may define expected values and meanings for this field,
                 *     and whether the values are considered a guaranteed API.
                 *     The value should be a CamelCase string.
                 *     This field may not be empty.
                 */
                reason: string;
                /**
                 * @description status of the condition, one of True, False, Unknown.
                 * @enum {string}
                 */
                status: "True" | "False" | "Unknown";
                /** @description type of condition in CamelCase or in foo.example.com/CamelCase. */
                type: string;
            }[];
            connectionState?: components["schemas"]["RepositoryStatusConnectionState"];
            /**
             * Format: int64
             * @description ObservedGeneration is the metadata.generation the status reflects.
             */
            observedGeneration?: number;
        };
        /** @description Spec is the credential template. */
        RepositoryCredentialSpec: {
            /**
             * @description AzureServicePrincipal authenticates with an Azure service principal; the client secret is read
             *     from the credentials Secret (key azureServicePrincipalClientSecret).
             */
            azureServicePrincipal?: components["schemas"]["RepositorySpecAzureServicePrincipal"];
            /**
             * @description CredentialsRef names a Secret in the same namespace holding credentials. Recognized keys:
             *     username, password, bearerToken, sshPrivateKey, tlsClientCertData, tlsClientCertKey,
             *     githubAppPrivateKey, gcpServiceAccountKey, azureServicePrincipalClientSecret.
             */
            credentialsRef?: components["schemas"]["RepositorySpecCredentialsRef"];
            /** @description EnableOCI enables Helm OCI support for a Helm repository. */
            enableOCI?: boolean;
            /** @description ForceHTTPBasicAuth sends basic auth on the first request instead of waiting for a challenge. */
            forceHTTPBasicAuth?: boolean;
            /**
             * @description GitHubApp authenticates as a GitHub App installation; the private key is read from the
             *     credentials Secret (key githubAppPrivateKey) unless SCMProviderRef supplies the App.
             */
            githubApp?: components["schemas"]["RepositorySpecGithubApp"];
            /** @description InsecureOCIForceHTTP connects to an OCI registry over plain HTTP (OCI only). */
            insecureOCIForceHTTP?: boolean;
            /** @description NoProxy is a comma-separated list of hosts that bypass Proxy. */
            noProxy?: string;
            /** @description Proxy is the HTTP(S) proxy URL used to reach the repository. */
            proxy?: string;
            /**
             * @description Purpose of the credentials.
             * @default read
             * @enum {string}
             */
            purpose?: "read" | "write";
            /**
             * @description SCMProviderRef obtains credentials (e.g. GitHub App installation tokens) from an SCMProvider in
             *     the same namespace.
             */
            scmProviderRef?: {
                /** @description Name of the SCMProvider. */
                name: string;
            };
            /**
             * @description Type of the matching repositories.
             * @default git
             * @enum {string}
             */
            type?: "git" | "helm" | "oci";
            /** @description URLPrefix selects the repositories the template applies to. */
            urlPrefix: string;
            /** @description UseAzureWorkloadIdentity authenticates with Azure Workload Identity. */
            useAzureWorkloadIdentity?: boolean;
        };
        /** @description Spec is the provider configuration. */
        SCMProviderSpec: {
            /** @description APIURL is the API base URL (provider default when empty, e.g. https://api.github.com). */
            apiURL?: string;
            /** @description Auth configures API authentication. Anonymous access when empty. */
            auth?: {
                /** @description BasicAuthSecretRef references a Secret with username and password keys. */
                basicAuthSecretRef?: components["schemas"]["RepositorySpecCredentialsRef"];
                /** @description GitHubApp authenticates as a GitHub App. */
                githubApp?: {
                    /**
                     * Format: int64
                     * @description AppID of the GitHub App.
                     */
                    appID: number;
                    /**
                     * Format: int64
                     * @description InstallationID of the App. When empty it is discovered per organization/repository.
                     */
                    installationID?: number;
                    /** @description PrivateKeySecretRef references the App's PEM private key. */
                    privateKeySecretRef: components["schemas"]["SCMProviderSpecCaSecretRef"];
                };
                /** @description TokenSecretRef references an access token. */
                tokenSecretRef?: components["schemas"]["SCMProviderSpecCaSecretRef"];
            };
            caSecretRef?: components["schemas"]["SCMProviderSpecCaSecretRef"];
            /** @description Insecure skips TLS certificate verification of the API. */
            insecure?: boolean;
            /** @description RateLimit bounds the request rate to the API. */
            rateLimit?: {
                /**
                 * Format: int32
                 * @description Burst is the maximum burst size.
                 */
                burst?: number;
                /**
                 * Format: int32
                 * @description RequestsPerSecond is the sustained request rate.
                 */
                requestsPerSecond: number;
            };
            /** @description StatusReporting configures commit status / check run reporting of sync results. */
            statusReporting?: {
                /**
                 * @description ContextPrefix prefixes the status context / check name.
                 * @default patrol-cd
                 */
                contextPrefix?: string;
                /** @description Enabled turns reporting on. */
                enabled?: boolean;
                /**
                 * @description Mode is checks (GitHub App only) or statuses.
                 * @default statuses
                 * @enum {string}
                 */
                mode?: "checks" | "statuses";
            };
            /**
             * @description Type of the SCM.
             * @enum {string}
             */
            type: "github" | "gitlab" | "gitea" | "bitbucketCloud" | "bitbucketServer" | "azureDevOps" | "awsCodeCommit" | "fake";
            /** @description WebURL is the web UI base URL used for links (provider default when empty). */
            webURL?: string;
            /** @description Webhook configures inbound webhook verification. */
            webhook?: {
                /** @description SecretRef references the shared webhook secret used to verify signatures. */
                secretRef: components["schemas"]["SCMProviderSpecCaSecretRef"];
            };
        };
        /** @description Status is the observed state of the provider. */
        SCMProviderStatus: {
            /**
             * @description Capabilities are the contract capabilities the provider implements (e.g. RepositoryLister,
             *     PullRequestLister, CheckRunReporter).
             */
            capabilities?: string[];
            /** @description Conditions are the current conditions (Ready, ConnectionFailed). */
            conditions?: {
                /**
                 * Format: date-time
                 * @description lastTransitionTime is the last time the condition transitioned from one status to another.
                 *     This should be when the underlying condition changed.  If that is not known, then using the time when the API field changed is acceptable.
                 */
                lastTransitionTime: string;
                /**
                 * @description message is a human readable message indicating details about the transition.
                 *     This may be an empty string.
                 */
                message: string;
                /**
                 * Format: int64
                 * @description observedGeneration represents the .metadata.generation that the condition was set based upon.
                 *     For instance, if .metadata.generation is currently 12, but the .status.conditions[x].observedGeneration is 9, the condition is out of date
                 *     with respect to the current state of the instance.
                 */
                observedGeneration?: number;
                /**
                 * @description reason contains a programmatic identifier indicating the reason for the condition's last transition.
                 *     Producers of specific condition types may define expected values and meanings for this field,
                 *     and whether the values are considered a guaranteed API.
                 *     The value should be a CamelCase string.
                 *     This field may not be empty.
                 */
                reason: string;
                /**
                 * @description status of the condition, one of True, False, Unknown.
                 * @enum {string}
                 */
                status: "True" | "False" | "Unknown";
                /** @description type of condition in CamelCase or in foo.example.com/CamelCase. */
                type: string;
            }[];
            /**
             * Format: date-time
             * @description LastCheckedAt is when the provider was last checked.
             */
            lastCheckedAt?: string;
            /**
             * Format: int64
             * @description ObservedGeneration is the metadata.generation the status reflects.
             */
            observedGeneration?: number;
            /** @description RateLimit is the API rate limit observed in the last response. */
            rateLimit?: {
                /**
                 * Format: int64
                 * @description Limit is the request quota of the window.
                 */
                limit?: number;
                /**
                 * Format: int64
                 * @description Remaining is the remaining quota.
                 */
                remaining?: number;
                /**
                 * Format: date-time
                 * @description ResetAt is when the quota resets.
                 */
                resetAt?: string;
            };
        };
    };
    responses: {
        /** @description Error (RFC 9457 problem details). */
        Problem: {
            headers: {
                [name: string]: unknown;
            };
            content: {
                "application/problem+json": components["schemas"]["Problem"];
            };
        };
    };
    parameters: never;
    requestBodies: never;
    headers: never;
    pathItems: never;
}
export type $defs = Record<string, never>;
export interface operations {
    getBadge: {
        parameters: {
            query?: {
                /** @description AppNamespace is the Application namespace (default: the control-plane namespace). Namespace is the upstream spelling, accepted as an alias. */
                appNamespace?: string;
                keepFullRevision?: boolean;
                /** @description Name is the Application. */
                name?: string;
                namespace?: string;
                /** @description Projects aggregates every Application of these projects (repeatable "project"). */
                project?: string[];
                /** @description Revision shows the last synced revision (7 characters unless KeepFullRevision). */
                revision?: boolean;
                /** @description ShowAppName adds a title row with the Application name. */
                showAppName?: boolean;
                /** @description Width overrides the badge width in pixels. */
                width?: number;
            };
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description The SVG badge. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "image/svg+xml": string;
                };
            };
            /** @description Status badges are disabled (statusbadge.enabled is not true). */
            404: {
                headers: {
                    [name: string]: unknown;
                };
                content?: never;
            };
            default: components["responses"]["Problem"];
        };
    };
    listAccounts: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["AccountList"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    canI: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                resource: string;
                action: string;
                /** @description May contain slashes. */
                subresource: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["CanIResponse"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    updatePassword: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["UpdatePasswordRequest"];
            };
        };
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["Empty"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    getAccount: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                name: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["Account"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    createAccountToken: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                name: string;
            };
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["CreateAccountTokenRequest"];
            };
        };
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["TokenResponse"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    deleteAccountToken: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                name: string;
                id: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["Empty"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    listApplications: {
        parameters: {
            query?: {
                /** @description AppNamespace restricts the query to one namespace (default: every enabled namespace). */
                appNamespace?: string;
                /** @description Continue is the opaque token of a previous list response. */
                continue?: string;
                /** @description Limit bounds the number of items of a list response (0: all). The response's metadata.continue resumes the listing. */
                limit?: number;
                /** @description Name keeps only the Application with this name. */
                name?: string;
                /** @description Project is the legacy spelling of Projects; when set it wins (upstream). */
                project?: string[];
                /** @description Projects keeps only Applications of these projects. */
                projects?: string[];
                /** @description Repo keeps only Applications with a source of this repository URL. */
                repo?: string;
                /** @description ResourceVersion: the watch stream skips Applications older than it and, without Name, sends no initial ADDED events. */
                resourceVersion?: string;
                /** @description Selector is a Kubernetes label selector. */
                selector?: string;
            };
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ApplicationList"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    createApplication: {
        parameters: {
            query?: {
                /** @description AppNamespace is used when the body has no metadata.namespace. */
                appNamespace?: string;
                /** @description Upsert updates an existing Application whose spec differs (requires update). */
                upsert?: boolean;
                /** @description Validate checks repositories and manifest generation (default true). */
                validate?: boolean;
            };
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["Application"];
            };
        };
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["StoredApplication"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    getApplication: {
        parameters: {
            query?: {
                appNamespace?: string;
                /** @description Project is the expected project (at most one value). With a project the answer for an Application the caller may not see in that project is 404, without one 403. */
                project?: string[];
                /** @description Projects is the newer spelling of Project. */
                projects?: string[];
                /** @description Refresh ("normal" or "hard") requests a refresh and waits (bounded) until the controller reconciled the Application. */
                refresh?: string;
                /** @description ResourceVersion reads the Application at this resource version. */
                resourceVersion?: string;
            };
            header?: never;
            path: {
                name: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["StoredApplication"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    updateApplication: {
        parameters: {
            query?: {
                appNamespace?: string;
                /** @description Project is the project the caller expects the Application to be in. */
                project?: string;
                validate?: boolean;
            };
            header?: never;
            path: {
                name: string;
            };
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["Application"];
            };
        };
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["StoredApplication"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    deleteApplication: {
        parameters: {
            query?: {
                appNamespace?: string;
                /** @description Cascade deletes the managed resources (default true). */
                cascade?: boolean;
                project?: string;
                /** @description PropagationPolicy is foreground (default) or background for cascading deletes. */
                propagationPolicy?: string;
            };
            header?: never;
            path: {
                name: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ApplicationResponse"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    patchApplication: {
        parameters: {
            query?: {
                appNamespace?: string;
                project?: string;
            };
            header?: never;
            path: {
                name: string;
            };
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["ApplicationPatchRequest"];
            };
        };
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["StoredApplication"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    listApplicationEvents: {
        parameters: {
            query?: {
                appNamespace?: string;
                project?: string;
                resourceName?: string;
                resourceNamespace?: string;
                resourceUID?: string;
            };
            header?: never;
            path: {
                name: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["EventList"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    listApplicationLinks: {
        parameters: {
            query?: {
                appNamespace?: string;
                namespace?: string;
                project?: string;
            };
            header?: never;
            path: {
                name: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["LinksResponse"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    getApplicationLogs: {
        parameters: {
            query?: {
                appNamespace?: string;
                container?: string;
                /** @description Download returns the lines as a text attachment instead of Server-Sent Events. */
                download?: boolean;
                /** @description Filter keeps lines containing the text (case-insensitive unless MatchCase); a leading "!" keeps lines that do not contain it. */
                filter?: string;
                follow?: boolean;
                group?: string;
                kind?: string;
                matchCase?: boolean;
                namespace?: string;
                podName?: string;
                previous?: boolean;
                project?: string;
                resourceName?: string;
                sinceSeconds?: number;
                /** @description SinceTime is an RFC 3339 time. */
                sinceTime?: string;
                tailLines?: number;
                /** @description UntilTime (RFC 3339, nanoseconds allowed) ends the stream at the first line after it. */
                untilTime?: string;
            };
            header?: never;
            path: {
                name: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description Server-Sent Events: one JSON document per data field; heartbeat comments every 15 s; an "error" event with a problem document ends a failed stream (x-patrol-events). With download=true: the lines as a text attachment. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/octet-stream": string;
                    "text/event-stream": components["schemas"]["LogEntry"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    getManagedResources: {
        parameters: {
            query?: {
                appNamespace?: string;
                group?: string;
                kind?: string;
                namespace?: string;
                project?: string;
                resourceName?: string;
                version?: string;
            };
            header?: never;
            path: {
                name: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ManagedResourcesResponse"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    getApplicationManifests: {
        parameters: {
            query?: {
                appNamespace?: string;
                /** @description NoCache bypasses the repo-server manifest cache. */
                noCache?: boolean;
                project?: string;
                /** @description Revision renders a single-source Application at this revision. */
                revision?: string;
                /** @description Revisions render the sources at SourcePositions (1-based) of a multi-source Application at these revisions. */
                revisions?: string[];
                sourcePositions?: number[];
            };
            header?: never;
            path: {
                name: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ApplicationManifestResponse"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    getApplicationManifestsWithFiles: {
        parameters: {
            query?: {
                appNamespace?: string;
                /** @description Checksum is the hex SHA-256 of the body; a mismatch is rejected. */
                checksum?: string;
                project?: string;
            };
            header?: never;
            path: {
                name: string;
            };
            cookie?: never;
        };
        /** @description Gzipped tar archive (binary) of the local source directory; application/x-gzip and application/octet-stream are accepted too, other media types get 415. */
        requestBody: {
            content: {
                "application/gzip": unknown;
            };
        };
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ApplicationManifestResponse"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    terminateApplicationOperation: {
        parameters: {
            query?: {
                appNamespace?: string;
                project?: string;
            };
            header?: never;
            path: {
                name: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ApplicationOperationResponse"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    listApplicationOperations: {
        parameters: {
            query?: {
                appNamespace?: string;
                /** @description Continue is the opaque token of a previous response. */
                continue?: string;
                /** @description Limit bounds the number of Operations returned (default 100, at most 1000), newest first. */
                limit?: number;
                project?: string;
            };
            header?: never;
            path: {
                name: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["OperationList"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    getApplicationOperation: {
        parameters: {
            query?: {
                appNamespace?: string;
                project?: string;
            };
            header?: never;
            path: {
                name: string;
                op: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["StoredOperation"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    getPodLogs: {
        parameters: {
            query?: {
                appNamespace?: string;
                container?: string;
                download?: boolean;
                filter?: string;
                follow?: boolean;
                matchCase?: boolean;
                namespace?: string;
                previous?: boolean;
                project?: string;
                sinceSeconds?: number;
                sinceTime?: string;
                tailLines?: number;
                untilTime?: string;
            };
            header?: never;
            path: {
                name: string;
                pod: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description Server-Sent Events: one JSON document per data field; heartbeat comments every 15 s; an "error" event with a problem document ends a failed stream (x-patrol-events). With download=true: the lines as a text attachment. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/octet-stream": string;
                    "text/event-stream": components["schemas"]["LogEntry"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    getResource: {
        parameters: {
            query?: {
                appNamespace?: string;
                group?: string;
                kind?: string;
                namespace?: string;
                project?: string;
                resourceName?: string;
                version?: string;
            };
            header?: never;
            path: {
                name: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ApplicationResourceResponse"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    patchResource: {
        parameters: {
            query?: {
                appNamespace?: string;
                group?: string;
                kind?: string;
                namespace?: string;
                project?: string;
                resourceName?: string;
                version?: string;
            };
            header?: never;
            path: {
                name: string;
            };
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["ApplicationResourcePatchRequest"];
            };
        };
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ApplicationResourceResponse"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    deleteResource: {
        parameters: {
            query?: {
                appNamespace?: string;
                force?: boolean;
                group?: string;
                kind?: string;
                namespace?: string;
                orphan?: boolean;
                project?: string;
                resourceName?: string;
                version?: string;
            };
            header?: never;
            path: {
                name: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ApplicationResponse"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    getResourceTree: {
        parameters: {
            query?: {
                appNamespace?: string;
                project?: string;
            };
            header?: never;
            path: {
                name: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ApplicationTree"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    listResourceActions: {
        parameters: {
            query?: {
                appNamespace?: string;
                group?: string;
                kind?: string;
                namespace?: string;
                project?: string;
                resourceName?: string;
                version?: string;
            };
            header?: never;
            path: {
                name: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ResourceActionsListResponse"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    runResourceAction: {
        parameters: {
            query?: {
                appNamespace?: string;
                group?: string;
                kind?: string;
                namespace?: string;
                project?: string;
                resourceName?: string;
                version?: string;
            };
            header?: never;
            path: {
                name: string;
            };
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["ResourceActionRunRequest"];
            };
        };
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ApplicationResponse"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    listApplicationResourceLinks: {
        parameters: {
            query?: {
                appNamespace?: string;
                group?: string;
                kind?: string;
                namespace?: string;
                project?: string;
                resourceName?: string;
                version?: string;
            };
            header?: never;
            path: {
                name: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["LinksResponse"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    getApplicationRevisionChartDetails: {
        parameters: {
            query?: {
                appNamespace?: string;
                project?: string;
                /** @description SourceIndex selects the source (0-based; default 0). */
                sourceIndex?: number;
                /** @description VersionID selects the sources of a status.history entry instead of the spec. */
                versionId?: number;
            };
            header?: never;
            path: {
                name: string;
                revision: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ApplicationChartDetails"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    getApplicationRevisionMetadata: {
        parameters: {
            query?: {
                appNamespace?: string;
                project?: string;
                /** @description SourceIndex selects the source (0-based; default 0). */
                sourceIndex?: number;
                /** @description VersionID selects the sources of a status.history entry instead of the spec. */
                versionId?: number;
            };
            header?: never;
            path: {
                name: string;
                revision: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ApplicationRevisionMetadata"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    getApplicationRevisionOCIMetadata: {
        parameters: {
            query?: {
                appNamespace?: string;
                project?: string;
                /** @description SourceIndex selects the source (0-based; default 0). */
                sourceIndex?: number;
                /** @description VersionID selects the sources of a status.history entry instead of the spec. */
                versionId?: number;
            };
            header?: never;
            path: {
                name: string;
                revision: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ApplicationOCIMetadata"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    rollbackApplication: {
        parameters: {
            query?: {
                appNamespace?: string;
                project?: string;
            };
            header?: never;
            path: {
                name: string;
            };
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["ApplicationRollbackRequest"];
            };
        };
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ApplicationOperationResponse"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    serverSideDiff: {
        parameters: {
            query?: {
                appNamespace?: string;
                project?: string;
            };
            header?: never;
            path: {
                name: string;
            };
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["ApplicationServerSideDiffRequest"];
            };
        };
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ServerSideDiffResponse"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    updateApplicationSpec: {
        parameters: {
            query?: {
                appNamespace?: string;
                project?: string;
                validate?: boolean;
            };
            header?: never;
            path: {
                name: string;
            };
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["ApplicationSpec"];
            };
        };
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ApplicationSpec"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    syncApplication: {
        parameters: {
            query?: {
                appNamespace?: string;
                project?: string;
            };
            header?: never;
            path: {
                name: string;
            };
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["ApplicationSyncRequest"];
            };
        };
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ApplicationOperationResponse"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    getApplicationSyncWindows: {
        parameters: {
            query?: {
                appNamespace?: string;
                project?: string;
            };
            header?: never;
            path: {
                name: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ApplicationSyncWindowsResponse"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    listApplicationSets: {
        parameters: {
            query?: {
                /** @description AppsetNamespace restricts the list to one namespace (default: every enabled namespace). */
                appsetNamespace?: string;
                /** @description Continue is the opaque token of a previous page's metadata.continue. */
                continue?: string;
                /** @description Limit bounds the number of items returned (0: no limit). */
                limit?: number;
                /** @description Projects keeps only ApplicationSets whose spec.template.spec.project is one of these (an empty project is "default"). */
                projects?: string[];
                /** @description Selector is a Kubernetes label selector. */
                selector?: string;
            };
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ApplicationSetList"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    createApplicationSet: {
        parameters: {
            query?: {
                /** @description DryRun renders the generated Applications into status.resources without persisting. */
                dryRun?: boolean;
                /** @description Upsert updates an existing ApplicationSet whose spec differs (requires update). */
                upsert?: boolean;
            };
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["ApplicationSet"];
            };
        };
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["StoredApplicationSet"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    generateApplicationSet: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["ApplicationSetGenerateRequest"];
            };
        };
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ApplicationSetGenerateResponse"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    getApplicationSet: {
        parameters: {
            query?: {
                /** @description AppsetNamespace is the ApplicationSet namespace (default: the control-plane namespace). */
                appsetNamespace?: string;
            };
            header?: never;
            path: {
                name: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["StoredApplicationSet"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    deleteApplicationSet: {
        parameters: {
            query?: {
                appsetNamespace?: string;
                /** @description Cascade (default true) deletes the generated Applications through their owner references; false orphans them. */
                cascade?: boolean;
                /** @description PropagationPolicy is "foreground" or "background" (default) for cascading deletes. */
                propagationPolicy?: string;
            };
            header?: never;
            path: {
                name: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["Empty"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    listApplicationSetEvents: {
        parameters: {
            query?: {
                /** @description AppsetNamespace is the ApplicationSet namespace (default: the control-plane namespace). */
                appsetNamespace?: string;
            };
            header?: never;
            path: {
                name: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["EventList"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    getApplicationSetResourceTree: {
        parameters: {
            query?: {
                /** @description AppsetNamespace is the ApplicationSet namespace (default: the control-plane namespace). */
                appsetNamespace?: string;
            };
            header?: never;
            path: {
                name: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ApplicationSetTree"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    ssoCallback: {
        parameters: {
            query?: {
                /** @description Code is the authorization code. */
                code?: string;
                /** @description Error is the identity provider's error code. */
                error?: string;
                /** @description ErrorDescription is the identity provider's error text. */
                error_description?: string;
                /** @description State must match the encrypted state cookie. */
                state?: string;
            };
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description Session cookie set; redirect to the return URL. */
            303: {
                headers: {
                    Location?: string;
                    "Set-Cookie"?: string;
                    [name: string]: unknown;
                };
                content?: never;
            };
            default: components["responses"]["Problem"];
        };
    };
    startSSOLogin: {
        parameters: {
            query?: {
                /** @description ReturnURL is where the browser goes after login; it must lie within url or one of additionalUrls. */
                return_url?: string;
            };
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description Redirect to the identity provider. */
            303: {
                headers: {
                    Location?: string;
                    "Set-Cookie"?: string;
                    [name: string]: unknown;
                };
                content?: never;
            };
            default: components["responses"]["Problem"];
        };
    };
    listCertificates: {
        parameters: {
            query?: {
                /** @description CertSubType filters SSH entries by key type. */
                certSubType?: string;
                /** @description CertType is "", "*", "ssh", "https" or "tls" (alias of https). */
                certType?: string;
                /** @description HostNamePattern is a file glob on the server name (empty matches everything; brackets are literal). */
                hostNamePattern?: string;
            };
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["RepositoryCertificateList"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    createCertificates: {
        parameters: {
            query?: {
                /** @description Upsert replaces existing entries with different data. */
                upsert?: boolean;
            };
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["RepositoryCertificateList"];
            };
        };
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["RepositoryCertificateList"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    deleteCertificates: {
        parameters: {
            query?: {
                /** @description CertSubType filters SSH entries by key type. */
                certSubType?: string;
                /** @description CertType is "", "*", "ssh", "https" or "tls" (alias of https). */
                certType?: string;
                /** @description HostNamePattern is a file glob on the server name (empty matches everything; brackets are literal). */
                hostNamePattern?: string;
            };
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["RepositoryCertificateList"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    listClusters: {
        parameters: {
            query?: {
                continue?: string;
                "id.type"?: string;
                "id.value"?: string;
                limit?: number;
                name?: string;
                /** @description Selector is a label selector on the Cluster objects. */
                selector?: string;
                server?: string;
            };
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["RegisteredClusterList"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    createCluster: {
        parameters: {
            query?: {
                /** @description Upsert updates an existing cluster whose configuration differs. */
                upsert?: boolean;
            };
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["ClusterInput"];
            };
        };
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["RegisteredCluster"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    getCluster: {
        parameters: {
            query?: {
                "id.type"?: string;
            };
            header?: never;
            path: {
                server: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["RegisteredCluster"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    updateCluster: {
        parameters: {
            query?: {
                "id.type"?: string;
                /** @description UpdatedFields limits the update to these paths (ClusterUpdatableFields; repeated or comma-separated). Empty replaces the whole cluster, including its credentials. */
                updatedFields?: string[];
            };
            header?: never;
            path: {
                server: string;
            };
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["ClusterInput"];
            };
        };
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["RegisteredCluster"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    deleteCluster: {
        parameters: {
            query?: {
                "id.type"?: string;
            };
            header?: never;
            path: {
                server: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["Empty"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    invalidateClusterCache: {
        parameters: {
            query?: {
                "id.type"?: string;
            };
            header?: never;
            path: {
                server: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["RegisteredCluster"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    rotateClusterAuth: {
        parameters: {
            query?: {
                "id.type"?: string;
            };
            header?: never;
            path: {
                server: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["Empty"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    listGPGKeys: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["GnuPGPublicKeyList"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    createGPGKeys: {
        parameters: {
            query?: {
                /** @description Upsert is accepted for compatibility and ignored: existing keys are always skipped. */
                upsert?: boolean;
            };
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["GnuPGPublicKey"];
            };
        };
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["GnuPGPublicKeyCreateResponse"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    getGPGKey: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                keyID: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["GnuPGPublicKey"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    deleteGPGKey: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                keyID: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["Empty"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    listNotificationServices: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["NotificationServiceList"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    listNotificationTemplates: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["NotificationTemplateList"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    listNotificationTriggers: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["NotificationTriggerList"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    listProjects: {
        parameters: {
            query?: {
                continue?: string;
                limit?: number;
                /** @description Selector is a label selector on the AppProjects. */
                selector?: string;
            };
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["AppProjectList"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    createProject: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["ProjectCreateRequest"];
            };
        };
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["StoredAppProject"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    getProject: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                name: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["StoredAppProject"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    updateProject: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                name: string;
            };
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["ProjectUpdateRequest"];
            };
        };
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["StoredAppProject"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    deleteProject: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                name: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["Empty"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    getDetailedProject: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                name: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ProjectDetailed"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    listProjectEvents: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                name: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["EventList"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    getGlobalProjects: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                name: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ProjectGlobalProjectsResponse"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    listProjectLinks: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                name: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["LinksResponse"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    getProjectSyncWindows: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                name: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ProjectSyncWindowsResponse"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    createProjectToken: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                project: string;
                role: string;
            };
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["ProjectTokenCreateRequest"];
            };
        };
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ProjectTokenResponse"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    deleteProjectToken: {
        parameters: {
            query?: {
                id?: string;
            };
            header?: never;
            path: {
                project: string;
                role: string;
                iat: number;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["Empty"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    listRepositoryCredentials: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["RepositoryCredentialInfoList"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    createRepositoryCredential: {
        parameters: {
            query?: {
                /** @description Upsert updates an existing template with the same URL prefix whose spec or credentials differ. */
                upsert?: boolean;
            };
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["RepositoryCredentialWriteRequest"];
            };
        };
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["RepositoryCredentialInfo"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    updateRepositoryCredential: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** @description URL is the URL prefix; it must equal spec.urlPrefix. */
                url: string;
            };
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["RepositoryCredentialWriteRequest"];
            };
        };
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["RepositoryCredentialInfo"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    deleteRepositoryCredential: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                url: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["Empty"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    listRepositories: {
        parameters: {
            query?: {
                /** @description AppProject lists only repositories scoped to this project. */
                appProject?: string;
                continue?: string;
                /** @description ForceRefresh re-checks the connection of every listed repository. */
                forceRefresh?: boolean;
                /** @description Limit and Continue paginate (items are filtered by RBAC, so a page may hold fewer than Limit items). */
                limit?: number;
                /** @description Selector is a label selector. */
                selector?: string;
            };
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["RepositoryInfoList"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    createRepository: {
        parameters: {
            query?: {
                /** @description CredsOnly creates a credential template (RepositoryCredential with urlPrefix = spec.url) instead of a repository. */
                credsOnly?: boolean;
                /** @description Upsert updates an existing repository with the same URL and project whose spec or credentials differ. */
                upsert?: boolean;
            };
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["RepositoryWriteRequest"];
            };
        };
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["RepositoryInfo"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    getRepository: {
        parameters: {
            query?: {
                /** @description AppProject selects the repository scoped to this project when several are registered for the URL. */
                appProject?: string;
                /** @description ForceRefresh re-checks the connection (GET only). */
                forceRefresh?: boolean;
            };
            header?: never;
            path: {
                /** @description Repo is the repository URL. */
                repo: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["RepositoryInfo"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    updateRepository: {
        parameters: {
            query?: {
                /** @description AppProject is the project the repository is currently scoped to, when it differs from spec.project (moving a repository between projects). */
                appProject?: string;
            };
            header?: never;
            path: {
                /** @description Repo is the repository URL; it must name the same repository as spec.url. */
                repo: string;
            };
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["RepositoryWriteRequest"];
            };
        };
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["RepositoryInfo"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    deleteRepository: {
        parameters: {
            query?: {
                /** @description AppProject selects the repository scoped to this project when several are registered for the URL. */
                appProject?: string;
                /** @description ForceRefresh re-checks the connection (GET only). */
                forceRefresh?: boolean;
            };
            header?: never;
            path: {
                /** @description Repo is the repository URL. */
                repo: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["Empty"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    getRepositoryAppDetails: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                repo: string;
            };
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["RepositoryAppDetailsQuery"];
            };
        };
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["RepositoryAppDetails"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    listRepositoryApps: {
        parameters: {
            query?: {
                /** @description AppName and AppProject name the Application the discovery is for; the caller needs applications create or update on it. */
                appName?: string;
                appProject?: string;
                /** @description Revision to inspect (default HEAD). */
                revision?: string;
            };
            header?: never;
            path: {
                repo: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["RepositoryAppList"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    listRepositoryHelmCharts: {
        parameters: {
            query?: {
                /** @description AppProject selects the repository scoped to this project when several are registered for the URL. */
                appProject?: string;
                /** @description ForceRefresh re-checks the connection (GET only). */
                forceRefresh?: boolean;
            };
            header?: never;
            path: {
                /** @description Repo is the repository URL. */
                repo: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["RepositoryHelmChartList"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    listRepositoryOCITags: {
        parameters: {
            query?: {
                /** @description AppProject selects the repository scoped to this project when several are registered for the URL. */
                appProject?: string;
                /** @description ForceRefresh re-checks the connection (GET only). */
                forceRefresh?: boolean;
            };
            header?: never;
            path: {
                /** @description Repo is the repository URL. */
                repo: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["RepositoryRefs"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    listRepositoryRefs: {
        parameters: {
            query?: {
                /** @description AppProject selects the repository scoped to this project when several are registered for the URL. */
                appProject?: string;
                /** @description ForceRefresh re-checks the connection (GET only). */
                forceRefresh?: boolean;
            };
            header?: never;
            path: {
                /** @description Repo is the repository URL. */
                repo: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["RepositoryRefs"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    validateRepositoryAccess: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** @description Repo is the repository URL (spec.url, when set, must name the same repository). */
                repo: string;
            };
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["RepositoryWriteRequest"];
            };
        };
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["RepositoryStatusConnectionState"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    listSCMProviders: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["SCMProviderList"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    createSCMProvider: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["SCMProviderWriteRequest"];
            };
        };
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["StoredSCMProvider"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    getSCMProvider: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                name: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["StoredSCMProvider"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    updateSCMProvider: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                name: string;
            };
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["SCMProviderWriteRequest"];
            };
        };
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["StoredSCMProvider"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    deleteSCMProvider: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                name: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["Empty"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    createSession: {
        parameters: {
            query?: {
                /** @description Cookie also sets the patrol.token session cookie (browser login). */
                cookie?: boolean;
            };
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["SessionCreateRequest"];
            };
        };
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["SessionResponse"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    deleteSession: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["LogoutResponse"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    exchangeOIDCToken: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["OIDCExchangeRequest"];
            };
        };
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["SessionResponse"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    getUserInfo: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["UserInfo"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    getSettings: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["Settings"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    getPluginSettings: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["PluginSettings"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    watchApplications: {
        parameters: {
            query?: {
                /** @description AppNamespace restricts the query to one namespace (default: every enabled namespace). */
                appNamespace?: string;
                /** @description Continue is the opaque token of a previous list response. */
                continue?: string;
                /** @description Limit bounds the number of items of a list response (0: all). The response's metadata.continue resumes the listing. */
                limit?: number;
                /** @description Name keeps only the Application with this name. */
                name?: string;
                /** @description Project is the legacy spelling of Projects; when set it wins (upstream). */
                project?: string[];
                /** @description Projects keeps only Applications of these projects. */
                projects?: string[];
                /** @description Repo keeps only Applications with a source of this repository URL. */
                repo?: string;
                /** @description ResourceVersion: the watch stream skips Applications older than it and, without Name, sends no initial ADDED events. */
                resourceVersion?: string;
                /** @description Selector is a Kubernetes label selector. */
                selector?: string;
            };
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description Server-Sent Events: one JSON document per data field; heartbeat comments every 15 s; an "error" event with a problem document ends a failed stream (x-patrol-events). */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "text/event-stream": components["schemas"]["WatchEvent_Application"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    watchResourceTree: {
        parameters: {
            query?: {
                appNamespace?: string;
                project?: string;
            };
            header?: never;
            path: {
                name: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description Server-Sent Events: one JSON document per data field; heartbeat comments every 15 s; an "error" event with a problem document ends a failed stream (x-patrol-events). */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "text/event-stream": components["schemas"]["WatchEvent_ApplicationTree"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    watchApplicationSets: {
        parameters: {
            query?: {
                appSetNamespace?: string;
                /** @description AppsetNamespace restricts the stream to one namespace. AppSetNamespace is the upstream spelling, accepted as an alias. */
                appsetNamespace?: string;
                /** @description Name restricts the stream to one ApplicationSet and sends its current state first. */
                name?: string;
                projects?: string[];
                /** @description ResourceVersion skips events of objects whose resourceVersion is older. */
                resourceVersion?: string;
                selector?: string;
            };
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description Server-Sent Events: one JSON document per data field; heartbeat comments every 15 s; an "error" event with a problem document ends a failed stream (x-patrol-events). */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "text/event-stream": components["schemas"]["ApplicationSetWatchEvent"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    getVersion: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["Version"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    listWriteRepositoryCredentials: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["RepositoryCredentialInfoList"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    createWriteRepositoryCredential: {
        parameters: {
            query?: {
                /** @description Upsert updates an existing template with the same URL prefix whose spec or credentials differ. */
                upsert?: boolean;
            };
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["RepositoryCredentialWriteRequest"];
            };
        };
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["RepositoryCredentialInfo"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    updateWriteRepositoryCredential: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** @description URL is the URL prefix; it must equal spec.urlPrefix. */
                url: string;
            };
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["RepositoryCredentialWriteRequest"];
            };
        };
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["RepositoryCredentialInfo"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    deleteWriteRepositoryCredential: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                url: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["Empty"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    listWriteRepositories: {
        parameters: {
            query?: {
                /** @description AppProject lists only repositories scoped to this project. */
                appProject?: string;
                continue?: string;
                /** @description ForceRefresh re-checks the connection of every listed repository. */
                forceRefresh?: boolean;
                /** @description Limit and Continue paginate (items are filtered by RBAC, so a page may hold fewer than Limit items). */
                limit?: number;
                /** @description Selector is a label selector. */
                selector?: string;
            };
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["RepositoryInfoList"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    createWriteRepository: {
        parameters: {
            query?: {
                /** @description CredsOnly creates a credential template (RepositoryCredential with urlPrefix = spec.url) instead of a repository. */
                credsOnly?: boolean;
                /** @description Upsert updates an existing repository with the same URL and project whose spec or credentials differ. */
                upsert?: boolean;
            };
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["RepositoryWriteRequest"];
            };
        };
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["RepositoryInfo"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    getWriteRepository: {
        parameters: {
            query?: {
                /** @description AppProject selects the repository scoped to this project when several are registered for the URL. */
                appProject?: string;
                /** @description ForceRefresh re-checks the connection (GET only). */
                forceRefresh?: boolean;
            };
            header?: never;
            path: {
                /** @description Repo is the repository URL. */
                repo: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["RepositoryInfo"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    updateWriteRepository: {
        parameters: {
            query?: {
                /** @description AppProject is the project the repository is currently scoped to, when it differs from spec.project (moving a repository between projects). */
                appProject?: string;
            };
            header?: never;
            path: {
                /** @description Repo is the repository URL; it must name the same repository as spec.url. */
                repo: string;
            };
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["RepositoryWriteRequest"];
            };
        };
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["RepositoryInfo"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    deleteWriteRepository: {
        parameters: {
            query?: {
                /** @description AppProject selects the repository scoped to this project when several are registered for the URL. */
                appProject?: string;
                /** @description ForceRefresh re-checks the connection (GET only). */
                forceRefresh?: boolean;
            };
            header?: never;
            path: {
                /** @description Repo is the repository URL. */
                repo: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["Empty"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    validateWriteRepositoryAccess: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** @description Repo is the repository URL (spec.url, when set, must name the same repository). */
                repo: string;
            };
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["RepositoryWriteRequest"];
            };
        };
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["RepositoryStatusConnectionState"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    getVersionAlias: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["Version"];
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    receiveWebhook: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /** @description The provider's event payload (JSON) as sent, at most webhook.maxPayloadSizeMB (default 50 MB, clamped to 25 MiB). */
        requestBody: {
            content: {
                "application/json": unknown;
            };
        };
        responses: {
            /** @description Accepted (queued), ignored (an event that refreshes nothing) or a duplicate delivery; empty body. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content?: never;
            };
            /** @description Unknown event (no provider matches the headers, or not the named one), invalid or too large payload, failed signature or token verification, unsupported event. */
            400: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "text/plain": string;
                };
            };
            /** @description Registry (ghcr) delivery whose signature verification failed. */
            401: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "text/plain": string;
                };
            };
            /** @description Too many deliveries from the source. */
            429: {
                headers: {
                    "Retry-After"?: string;
                    [name: string]: unknown;
                };
                content: {
                    "text/plain": string;
                };
            };
            /** @description Busy (request body budget or event queue full), or the webhook secrets are not loaded yet or unavailable. */
            503: {
                headers: {
                    "Retry-After"?: string;
                    [name: string]: unknown;
                };
                content: {
                    "text/plain": string;
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    receiveProviderWebhook: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** @description Provider selects the receiver (matched case-insensitively; see x-patrol-webhook-receivers). The delivery's headers must still identify that provider, otherwise the answer is 400. Unknown names get 404. */
                provider: "azuredevops" | "gitea" | "gogs" | "github" | "gitlab" | "bitbucket" | "bitbucketserver" | "ghcr" | "azure-devops" | "bitbucketcloud" | "bitbucket-cloud" | "bitbucket-server";
            };
            cookie?: never;
        };
        /** @description The provider's event payload (JSON) as sent, at most webhook.maxPayloadSizeMB (default 50 MB, clamped to 25 MiB). */
        requestBody: {
            content: {
                "application/json": unknown;
            };
        };
        responses: {
            /** @description Accepted (queued), ignored (an event that refreshes nothing) or a duplicate delivery; empty body. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content?: never;
            };
            /** @description Unknown event (no provider matches the headers, or not the named one), invalid or too large payload, failed signature or token verification, unsupported event. */
            400: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "text/plain": string;
                };
            };
            /** @description Registry (ghcr) delivery whose signature verification failed. */
            401: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "text/plain": string;
                };
            };
            /** @description Unknown receiver name. */
            404: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "text/plain": string;
                };
            };
            /** @description Too many deliveries from the source. */
            429: {
                headers: {
                    "Retry-After"?: string;
                    [name: string]: unknown;
                };
                content: {
                    "text/plain": string;
                };
            };
            /** @description Busy (request body budget or event queue full), or the webhook secrets are not loaded yet or unavailable. */
            503: {
                headers: {
                    "Retry-After"?: string;
                    [name: string]: unknown;
                };
                content: {
                    "text/plain": string;
                };
            };
            default: components["responses"]["Problem"];
        };
    };
    invokeExtensionGet: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** @description Extension is the extension name. */
                extension: string;
                /** @description Path is forwarded to the backend (may contain slashes). May contain slashes. */
                path: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description The backend's response (any status and media type). */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "*/*": string;
                };
            };
            /** @description The backend could not be reached or answered invalidly. */
            502: {
                headers: {
                    [name: string]: unknown;
                };
                content?: never;
            };
            /** @description The backend did not answer in time. */
            504: {
                headers: {
                    [name: string]: unknown;
                };
                content?: never;
            };
            default: components["responses"]["Problem"];
        };
    };
    invokeExtensionPut: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** @description Extension is the extension name. */
                extension: string;
                /** @description Path is forwarded to the backend (may contain slashes). May contain slashes. */
                path: string;
            };
            cookie?: never;
        };
        /** @description Opaque body forwarded unchanged to the backend (any media type; Content-Type is forwarded). Bodies over the proxy's request limit are refused with 413. */
        requestBody?: {
            content: {
                "*/*": unknown;
            };
        };
        responses: {
            /** @description The backend's response (any status and media type). */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "*/*": string;
                };
            };
            /** @description The backend could not be reached or answered invalidly. */
            502: {
                headers: {
                    [name: string]: unknown;
                };
                content?: never;
            };
            /** @description The backend did not answer in time. */
            504: {
                headers: {
                    [name: string]: unknown;
                };
                content?: never;
            };
            default: components["responses"]["Problem"];
        };
    };
    invokeExtensionPost: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** @description Extension is the extension name. */
                extension: string;
                /** @description Path is forwarded to the backend (may contain slashes). May contain slashes. */
                path: string;
            };
            cookie?: never;
        };
        /** @description Opaque body forwarded unchanged to the backend (any media type; Content-Type is forwarded). Bodies over the proxy's request limit are refused with 413. */
        requestBody?: {
            content: {
                "*/*": unknown;
            };
        };
        responses: {
            /** @description The backend's response (any status and media type). */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "*/*": string;
                };
            };
            /** @description The backend could not be reached or answered invalidly. */
            502: {
                headers: {
                    [name: string]: unknown;
                };
                content?: never;
            };
            /** @description The backend did not answer in time. */
            504: {
                headers: {
                    [name: string]: unknown;
                };
                content?: never;
            };
            default: components["responses"]["Problem"];
        };
    };
    invokeExtensionDelete: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** @description Extension is the extension name. */
                extension: string;
                /** @description Path is forwarded to the backend (may contain slashes). May contain slashes. */
                path: string;
            };
            cookie?: never;
        };
        /** @description Opaque body forwarded unchanged to the backend (any media type; Content-Type is forwarded). Bodies over the proxy's request limit are refused with 413. */
        requestBody?: {
            content: {
                "*/*": unknown;
            };
        };
        responses: {
            /** @description The backend's response (any status and media type). */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "*/*": string;
                };
            };
            /** @description The backend could not be reached or answered invalidly. */
            502: {
                headers: {
                    [name: string]: unknown;
                };
                content?: never;
            };
            /** @description The backend did not answer in time. */
            504: {
                headers: {
                    [name: string]: unknown;
                };
                content?: never;
            };
            default: components["responses"]["Problem"];
        };
    };
    invokeExtensionPatch: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** @description Extension is the extension name. */
                extension: string;
                /** @description Path is forwarded to the backend (may contain slashes). May contain slashes. */
                path: string;
            };
            cookie?: never;
        };
        /** @description Opaque body forwarded unchanged to the backend (any media type; Content-Type is forwarded). Bodies over the proxy's request limit are refused with 413. */
        requestBody?: {
            content: {
                "*/*": unknown;
            };
        };
        responses: {
            /** @description The backend's response (any status and media type). */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "*/*": string;
                };
            };
            /** @description The backend could not be reached or answered invalidly. */
            502: {
                headers: {
                    [name: string]: unknown;
                };
                content?: never;
            };
            /** @description The backend did not answer in time. */
            504: {
                headers: {
                    [name: string]: unknown;
                };
                content?: never;
            };
            default: components["responses"]["Problem"];
        };
    };
    openTerminal: {
        parameters: {
            query?: {
                appName?: string;
                appNamespace?: string;
                container?: string;
                namespace?: string;
                pod?: string;
                projectName?: string;
                /** @description Shell is used when it is in exec.shells; otherwise the configured shells are tried in order. */
                shell?: string;
            };
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description WebSocket session. */
            101: {
                headers: {
                    [name: string]: unknown;
                };
                content?: never;
            };
            /** @description OK */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["TerminalMessage"];
                };
            };
            /** @description exec.enabled is not true. */
            404: {
                headers: {
                    [name: string]: unknown;
                };
                content?: never;
            };
            default: components["responses"]["Problem"];
        };
    };
}
type WithRequired<T, K extends keyof T> = T & {
    [P in K]-?: T[P];
};
