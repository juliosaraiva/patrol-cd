/**
 * Notification subscription writes (NOTIF-109): merge-patch an Application's annotations so
 * only subscription keys change (other annotations and concurrent edits are untouched).
 */
import { useMutation, useQueryClient } from '@tanstack/react-query';
import * as apps from '../endpoints/applications';
import { canonicalNamespace, controlPlaneNamespace, queryKeys } from '../queries/keys';
import type { Application, ApplicationList } from '../types';

export interface AnnotationPatch {
  name: string;
  namespace?: string | undefined;
  project?: string | undefined;
  /** Annotation values to set; null removes the key. */
  annotations: Record<string, string | null>;
}

export function usePatchApplicationAnnotations() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: ({ name, namespace, project, annotations }: AnnotationPatch) =>
      apps.patchApplication(
        name,
        { patchType: 'merge', patch: JSON.stringify({ metadata: { annotations } }) },
        { appNamespace: namespace, project },
      ),
    onSuccess: (app: Application) => {
      const ns = canonicalNamespace(app.metadata.namespace, controlPlaneNamespace(queryClient));
      queryClient.setQueryData(queryKeys.applications.detail(app.metadata.name ?? '', { appNamespace: ns }), app);
      queryClient.setQueriesData<ApplicationList>({ queryKey: queryKeys.applications.lists() }, (old) =>
        old
          ? {
              ...old,
              items: old.items.map((item) =>
                item.metadata.name === app.metadata.name && item.metadata.namespace === app.metadata.namespace ? app : item,
              ),
            }
          : old,
      );
    },
  });
}
