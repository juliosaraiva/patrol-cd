/**
 * Proxy extension calls (API-046..049): requests to /extensions/<name>/<path> under the root
 * path, authorized by the session cookie plus the application named in the headers.
 *
 * Header contract (Patrol names of Argo CD's Argocd-Application-Name / Argocd-Project-Name):
 *   Patrol-Application-Name: <application namespace>:<application name>
 *   Patrol-Project-Name:     <project>
 */
import { invokeExtension, type ExtensionCall } from '../api/endpoints/misc';
import type { Application } from '../api/types';

export const APPLICATION_HEADER = 'Patrol-Application-Name';
export const PROJECT_HEADER = 'Patrol-Project-Name';

/** Namespace used when an Application object carries none (control-plane namespace). */
function applicationNamespace(app: Application, controlPlaneNamespace?: string): string {
  return app.metadata.namespace ?? controlPlaneNamespace ?? '';
}

/** Headers identifying `app` for a proxy extension request. */
export function extensionProxyHeaders(app: Application, controlPlaneNamespace?: string): Record<string, string> {
  return {
    [APPLICATION_HEADER]: `${applicationNamespace(app, controlPlaneNamespace)}:${app.metadata.name ?? ''}`,
    [PROJECT_HEADER]: app.spec.project,
  };
}

/**
 * Calls a proxy extension on behalf of `app`. Errors are ApiErrors like every other call
 * (401 "Unauthorized extension request" when RBAC denies `extensions, invoke, <name>`).
 */
export function callProxyExtension<T = unknown>(
  extension: string,
  path: string,
  app: Application,
  call: Omit<ExtensionCall, 'application' | 'project'> & { controlPlaneNamespace?: string } = {},
): Promise<T> {
  const { controlPlaneNamespace, ...rest } = call;
  return invokeExtension<T>(extension, path, {
    ...rest,
    application: { namespace: applicationNamespace(app, controlPlaneNamespace), name: app.metadata.name ?? '' },
    project: app.spec.project,
  });
}
