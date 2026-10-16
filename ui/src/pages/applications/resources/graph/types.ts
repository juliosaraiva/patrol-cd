import type { ResourceItem } from '../model/resources';

/** Node kinds drawn by the graph canvas. */
export type GraphNodeType = 'app' | 'resource' | 'group' | 'more' | 'traffic';

export interface GraphNode {
  id: string;
  type: GraphNodeType;
  /** Visible and accessible name (resource name, "12 Pods", "Show 40 more", ...). */
  label: string;
  /** Kubernetes kind for the icon and caption ('' when not a resource). */
  kind: string;
  /** Resource behind a 'resource' node. */
  item?: ResourceItem | undefined;
  /** Members of a 'group' node. */
  members?: ResourceItem[] | undefined;
  /** Expansion key of a 'group' node, or the parent id of a 'more' node. */
  expandKey?: string | undefined;
  /** Children hidden behind a 'more' node. */
  hiddenCount?: number | undefined;
  /** Health/sync shown on the node (app root, resources). */
  health?: string | undefined;
  sync?: string | undefined;
  /** Secondary line (namespace, endpoint, ...). */
  subtitle?: string | undefined;
  /** Shown only as the ancestor of a filter match. */
  dimmed?: boolean | undefined;
  /** True/false for nodes with children (expanded or collapsed), undefined for leaves. */
  expanded?: boolean | undefined;
  /** Descendants hidden because the node is collapsed. */
  collapsedCount?: number | undefined;
}

export interface GraphEdge {
  from: string;
  to: string;
  /** Drawn dashed: links to orphaned resources. */
  dashed?: boolean | undefined;
}

export interface Graph {
  nodes: GraphNode[];
  edges: GraphEdge[];
  roots: string[];
}
