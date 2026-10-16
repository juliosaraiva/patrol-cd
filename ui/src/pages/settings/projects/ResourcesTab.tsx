import { useState } from 'react';
import { Mono, Table } from '../../../design';
import { groupSuggestions, kindSuggestions } from '../common/catalog';
import { RowListEditor, Suggestions, type Row } from '../common/editors';
import { useFormErrors } from '../common/feedback';
import styles from '../common/common.module.css';
import { ProjectSection, SectionDrawer, useProjectSave, type ProjectContext } from './shared';

type ListKey = 'clusterResourceWhitelist' | 'clusterResourceBlacklist' | 'namespaceResourceWhitelist' | 'namespaceResourceBlacklist';

interface ListDef {
  key: ListKey;
  title: string;
  description: string;
  empty: string;
  clusterScoped: boolean;
}

const LISTS: readonly ListDef[] = [
  {
    key: 'clusterResourceWhitelist',
    title: 'Cluster resource allow list',
    description: 'Cluster-scoped kinds applications may manage. When empty, no cluster-scoped resource is allowed.',
    empty: 'Empty: cluster-scoped resources are denied.',
    clusterScoped: true,
  },
  {
    key: 'clusterResourceBlacklist',
    title: 'Cluster resource deny list',
    description: 'Cluster-scoped kinds that are denied even when the allow list matches them.',
    empty: 'Nothing denied.',
    clusterScoped: true,
  },
  {
    key: 'namespaceResourceWhitelist',
    title: 'Namespace resource allow list',
    description: 'Namespaced kinds applications may manage. When empty, every namespaced kind is allowed unless denied.',
    empty: 'Empty: all namespaced kinds are allowed.',
    clusterScoped: false,
  },
  {
    key: 'namespaceResourceBlacklist',
    title: 'Namespace resource deny list',
    description: 'Namespaced kinds applications may not manage.',
    empty: 'Nothing denied.',
    clusterScoped: false,
  },
];

interface Entry {
  group: string;
  kind: string;
  name?: string;
}

const groupLabel = (g: string) => (g === '' ? 'core' : g);

/** Resources tab: cluster and namespace resource allow and deny lists (API-105, TEN-064). */
export function ResourcesTab({ ctx }: { ctx: ProjectContext }) {
  const spec = ctx.project.spec;
  const saver = useProjectSave(ctx);
  const form = useFormErrors();
  const [editing, setEditing] = useState<ListDef | null>(null);
  const [rows, setRows] = useState<Row[]>([]);

  const entries = (key: ListKey): Entry[] => (spec[key] ?? []);
  const open = (def: ListDef) => {
    form.reset();
    saver.reset();
    setRows(entries(def.key).map((e) => ({ group: e.group, kind: e.kind, ...(def.clusterScoped ? { name: e.name ?? '' } : {}) })));
    setEditing(def);
  };
  const errors = rows.map((r) => ((r.kind ?? '').trim() === '' ? 'Enter a kind, or * for any kind.' : null));

  const submit = () => {
    if (!editing) return;
    const def = editing;
    if (!form.check(errors.flatMap((e, i) => (e ? [{ id: `resources-${i}-group`, message: `Entry ${i + 1}: ${e}` }] : [])))) return;
    const next = rows.map((r) => {
      const entry: Entry = { group: (r.group ?? '').trim(), kind: (r.kind ?? '').trim() };
      if (def.clusterScoped && (r.name ?? '').trim()) entry.name = (r.name ?? '').trim();
      return entry;
    });
    saver.save((s) => ({ ...s, [def.key]: next }), `${def.title} updated`, () => setEditing(null));
  };

  return (
    <div className={styles.stack}>
      <p className={styles.muted}>
        Deny lists always win. Group <Mono>*</Mono> and kind <Mono>*</Mono> match anything; an empty group is the core API group.
      </p>
      {LISTS.map((def) => {
        const items = entries(def.key);
        return (
          <ProjectSection key={def.key} id={`project-${def.key}`} title={def.title} description={def.description} onEdit={() => open(def)} editLabel={`Edit ${def.title.toLowerCase()}`}>
            {items.length === 0 ? (
              <p className={styles.muted}>{def.empty}</p>
            ) : (
              <Table
                caption={def.title}
                rows={items}
                getRowKey={(e) => `${e.group}/${e.kind}/${e.name ?? ''}`}
                columns={[
                  { id: 'group', header: 'Group', cell: (e) => <Mono>{groupLabel(e.group)}</Mono> },
                  { id: 'kind', header: 'Kind', cell: (e) => <Mono>{e.kind}</Mono> },
                  ...(def.clusterScoped ? [{ id: 'name', header: 'Name', cell: (e: Entry) => (e.name ? <Mono>{e.name}</Mono> : 'Any') }] : []),
                ]}
              />
            )}
          </ProjectSection>
        );
      })}
      {editing ? (
        <SectionDrawer
          title={`Edit ${editing.title.toLowerCase()}`}
          description={editing.description}
          onClose={() => setEditing(null)}
          onSubmit={submit}
          pending={saver.pending}
          error={saver.error}
          onReload={ctx.reload}
          errors={form.errors}
          attempt={form.attempt}
        >
          <Suggestions id="project-resource-groups" values={groupSuggestions(!editing.clusterScoped)} />
          <Suggestions id="project-resource-kinds" values={kindSuggestions(!editing.clusterScoped)} />
          <RowListEditor
            legend={editing.title}
            noun="entry"
            idPrefix="resources"
            rows={rows}
            onChange={setRows}
            fields={[
              { key: 'group', label: 'Group', mono: true, placeholder: 'core', list: 'project-resource-groups' },
              { key: 'kind', label: 'Kind', mono: true, placeholder: 'Deployment', list: 'project-resource-kinds' },
              ...(editing.clusterScoped ? [{ key: 'name', label: 'Name', mono: true, placeholder: 'any' }] : []),
            ]}
            errors={form.submitted ? errors : undefined}
            help="Suggestions list common kinds; any group and kind can be typed. Leave the group empty for core kinds such as Namespace."
            emptyText={editing.clusterScoped && editing.key === 'clusterResourceWhitelist' ? 'No cluster-scoped kinds are allowed.' : 'No entries.'}
          />
        </SectionDrawer>
      ) : null}
    </div>
  );
}
