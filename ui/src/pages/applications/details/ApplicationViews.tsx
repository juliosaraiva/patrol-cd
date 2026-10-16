import { useSearchParams } from 'react-router';
import { useResourceTree } from '../../../api/queries/applications';
import type { Application, AppScope } from '../../../api/types';
import { Tabs } from '../../../design';
import { AppViewExtensions, IconExtension, useAppViewExtensions } from '../../../extensions';
import styles from './Details.module.css';
import { BUILT_IN_VIEWS, DEFAULT_VIEW_ANNOTATION, resolveBuiltInView, ResourceViewsSlot, VIEW_PARAM, type ResourceViewsProps } from './ResourceViewsSlot';

const EXTENSION_TAB_PREFIX = 'extension:';

/**
 * One app view extension with the live resource tree. The resource views are not mounted while
 * an extension view is shown, so this is the only watcher of the tree then.
 */
function ExtensionView({ app, scope, title }: { app: Application; scope: AppScope; title: string }) {
  const tree = useResourceTree(app.metadata.name ?? '', scope);
  return <AppViewExtensions application={app} tree={tree.data} view={title} />;
}

/**
 * Views of the application details page: the resource views (tree, pods, network, list) and
 * the app view extensions (registerAppViewExtension, API-123) whose shouldDisplay accepts the
 * application. Without such extensions the resource views render their own switcher. With them,
 * this component renders one switcher offering both, bound to `?view=` (a built-in view id or
 * the extension title, as upstream); the default-view annotation may also name an extension.
 */
export function ApplicationViews(props: Omit<ResourceViewsProps, 'view'>) {
  const { app, scope } = props;
  const [params, setParams] = useSearchParams();
  // `?view=` names an extension by title: the first registration of a title wins.
  const extensions = useAppViewExtensions(app).filter((ext, index, all) => all.findIndex((e) => e.title === ext.title) === index);
  if (extensions.length === 0) return <ResourceViewsSlot {...props} />;

  const param = params.get(VIEW_PARAM);
  const requested = param ?? app.metadata.annotations?.[DEFAULT_VIEW_ANNOTATION];
  const extension = extensions.find((e) => e.title === requested);
  const builtIn = resolveBuiltInView(app, param);
  const selected = extension ? `${EXTENSION_TAB_PREFIX}${extension.title}` : builtIn;

  const select = (id: string) =>
    setParams((prev) => {
      const next = new URLSearchParams(prev);
      next.set(VIEW_PARAM, id.startsWith(EXTENSION_TAB_PREFIX) ? id.slice(EXTENSION_TAB_PREFIX.length) : id);
      return next;
    });

  return (
    <Tabs
      label="Application views"
      value={selected}
      onValueChange={select}
      items={[
        ...BUILT_IN_VIEWS.map((v) => ({
          id: v.id,
          label: v.label,
          content: v.id === selected ? <ResourceViewsSlot {...props} view={v.id} /> : null,
        })),
        ...extensions.map((ext) => {
          const id = `${EXTENSION_TAB_PREFIX}${ext.title}`;
          return {
            id,
            label: (
              <span className={styles.viewTab}>
                <IconExtension />
                {ext.title}
              </span>
            ),
            content: id === selected ? <ExtensionView app={app} scope={scope} title={ext.title} /> : null,
          };
        }),
      ]}
    />
  );
}
