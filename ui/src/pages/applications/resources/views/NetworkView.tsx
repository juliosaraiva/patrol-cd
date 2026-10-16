import { useMemo } from 'react';
import { EmptyState, Mono, Table, type Column } from '../../../../design';
import { IconExternal } from '../../../../design/icons';
import { isHttpURL } from '../../../../lib/url';
import { GraphCanvas } from '../graph/GraphCanvas';
import type { GraphNode } from '../graph/types';
import { buildNetworkGraph, type ExternalEndpoint } from '../model/networkGraph';
import { usePref } from '../model/prefs';
import type { ResourceItem, ResourceModel } from '../model/resources';
import styles from './Views.module.css';

export interface NetworkViewProps {
  appName: string;
  model: ResourceModel;
  matcher: (item: ResourceItem) => boolean;
  selectedId: string | undefined;
  onSelect: (item: ResourceItem) => void;
}

/** External traffic → Ingress → Service → Pod relationships from the tree's networking info. */
export function NetworkView({ appName, model, matcher, selectedId, onSelect }: NetworkViewProps) {
  const [zoom, setZoom] = usePref('zoom');
  const graph = useMemo(() => buildNetworkGraph(model, matcher), [model, matcher]);
  const onActivate = (node: GraphNode) => {
    if (node.item) onSelect(node.item);
  };

  if (graph.nodes.length === 0) {
    return (
      <EmptyState title="No networking resources" headingLevel={3}>
        <p>
          Services, Ingresses (or other routes) and the Pods they select appear here once the application deploys them. Clear the filters if
          you expected to see some.
        </p>
      </EmptyState>
    );
  }

  const columns: Column<ExternalEndpoint>[] = [
    { id: 'resource', header: 'Resource', cell: (e) => <Mono>{`${e.item.kind}/${e.item.name}`}</Mono>, sortValue: (e) => `${e.item.kind}/${e.item.name}` },
    {
      id: 'addresses',
      header: 'Load balancer',
      cell: (e) => (e.addresses.length > 0 ? <Mono>{e.addresses.join(', ')}</Mono> : '—'),
    },
    {
      id: 'urls',
      header: 'External URLs',
      cell: (e) =>
        e.urls.length > 0 ? (
          <ul className={styles.plainList}>
            {e.urls.map((url) =>
              isHttpURL(url) ? (
                <li key={url}>
                  <a href={url} target="_blank" rel="noopener noreferrer" className={styles.externalLink}>
                    <Mono>{url}</Mono>
                    <IconExternal size={12} />
                    <span className="sr-only"> (opens in a new tab)</span>
                  </a>
                </li>
              ) : (
                <li key={url}>
                  <Mono>{url}</Mono>
                </li>
              ),
            )}
          </ul>
        ) : (
          '—'
        ),
    },
  ];

  return (
    <div className={styles.viewBody}>
      <GraphCanvas
        graph={graph}
        label={`Network of ${appName}`}
        selectedId={selectedId}
        onActivate={onActivate}
        zoom={zoom}
        onZoomChange={setZoom}
      />
      {graph.endpoints.length > 0 ? (
        <section aria-label="External endpoints" className={styles.section}>
          <h3 className={styles.sectionTitle}>External endpoints</h3>
          <Table
            caption="External endpoints"
            columns={columns}
            rows={graph.endpoints}
            getRowKey={(e) => e.item.key}
            onRowActivate={(e) => onSelect(e.item)}
          />
        </section>
      ) : null}
    </div>
  );
}
