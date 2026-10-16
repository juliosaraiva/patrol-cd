import { useCallback, useMemo, useState } from 'react';
import type { Application } from '../../../../api/types';
import { Switch } from '../../../../design';
import { GraphCanvas } from '../graph/GraphCanvas';
import type { GraphNode } from '../graph/types';
import { usePref } from '../model/prefs';
import type { ResourceItem, ResourceModel } from '../model/resources';
import { buildTreeGraph } from '../model/treeGraph';
import { OutlineTree } from './OutlineTree';
import styles from './Views.module.css';

export interface TreeViewProps {
  app: Application;
  model: ResourceModel;
  matcher: (item: ResourceItem) => boolean;
  filtering: boolean;
  selectedId: string | undefined;
  onSelect: (item: ResourceItem) => void;
  onOpenApplication?: (() => void) | undefined;
}

function toggled(set: ReadonlySet<string>, key: string, on: boolean): Set<string> {
  const next = new Set(set);
  if (on) next.add(key);
  else next.delete(key);
  return next;
}

/** Application → managed resources → children, as a zoomable graph or a text outline. */
export function TreeView({ app, model, matcher, filtering, selectedId, onSelect, onOpenApplication }: TreeViewProps) {
  const [groupSimilar, setGroupSimilar] = usePref('groupSimilar');
  const [zoom, setZoom] = usePref('zoom');
  const [outline, setOutline] = useState(false);
  const [collapsed, setCollapsed] = useState<ReadonlySet<string>>(() => new Set());
  const [expandedParents, setExpandedParents] = useState<ReadonlySet<string>>(() => new Set());
  const [expandedGroups, setExpandedGroups] = useState<ReadonlySet<string>>(() => new Set());

  const graph = useMemo(
    () => buildTreeGraph(app, model, { matcher, filtering, collapsed, expandedParents, expandedGroups, groupSimilar }),
    [app, model, matcher, filtering, collapsed, expandedParents, expandedGroups, groupSimilar],
  );

  const onActivate = useCallback(
    (node: GraphNode) => {
      switch (node.type) {
        case 'app':
          onOpenApplication?.();
          break;
        case 'resource':
          if (node.item) onSelect(node.item);
          break;
        case 'group':
          if (node.expandKey) setExpandedGroups((s) => toggled(s, node.expandKey ?? '', true));
          break;
        case 'more':
          if (node.expandKey) setExpandedParents((s) => toggled(s, node.expandKey ?? '', true));
          break;
        case 'traffic':
          break;
      }
    },
    [onOpenApplication, onSelect],
  );

  const onToggle = useCallback((node: GraphNode, expand: boolean) => {
    if (node.type === 'group') {
      if (expand && node.expandKey) setExpandedGroups((s) => toggled(s, node.expandKey ?? '', true));
      return;
    }
    if (node.type === 'resource') setCollapsed((s) => toggled(s, node.id, !expand));
  }, []);

  const label = `Resource tree of ${app.metadata.name ?? ''}`;
  return (
    <div className={styles.viewBody}>
      <div className={styles.viewOptions}>
        <Switch checked={groupSimilar} onChange={setGroupSimilar} label="Group similar resources" />
        <Switch checked={outline} onChange={setOutline} label="Show as outline" />
        {collapsed.size > 0 || expandedGroups.size > 0 || expandedParents.size > 0 ? (
          <button
            type="button"
            className={styles.linkButton}
            onClick={() => {
              setCollapsed(new Set());
              setExpandedGroups(new Set());
              setExpandedParents(new Set());
            }}
          >
            Reset expanded and collapsed nodes
          </button>
        ) : null}
      </div>
      {outline ? (
        <OutlineTree graph={graph} label={label} selectedId={selectedId} onActivate={onActivate} onToggle={onToggle} />
      ) : (
        <GraphCanvas graph={graph} label={label} selectedId={selectedId} onActivate={onActivate} onToggle={onToggle} zoom={zoom} onZoomChange={setZoom} />
      )}
    </div>
  );
}
