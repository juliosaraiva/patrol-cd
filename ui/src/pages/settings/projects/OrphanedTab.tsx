import { useState } from 'react';
import { Checkbox, KeyValueList, Mono, Switch, Table } from '../../../design';
import { groupSuggestions, kindSuggestions } from '../common/catalog';
import { orDefault, RowListEditor, Suggestions, type Row } from '../common/editors';
import { useFormErrors } from '../common/feedback';
import styles from '../common/common.module.css';
import { ProjectSection, SectionDrawer, useProjectSave, type ProjectContext } from './shared';

/** Orphaned resources monitoring: enable, warn, ignore list (API-105, TEN-064). */
export function OrphanedTab({ ctx }: { ctx: ProjectContext }) {
  const orphaned = ctx.project.spec.orphanedResources;
  const enabled = orphaned !== undefined;
  const saver = useProjectSave(ctx);
  const form = useFormErrors();
  const [editing, setEditing] = useState(false);
  const [on, setOn] = useState(false);
  const [warn, setWarn] = useState(false);
  const [ignore, setIgnore] = useState<Row[]>([]);

  const open = () => {
    form.reset();
    saver.reset();
    setOn(enabled);
    setWarn(orphaned?.warn ?? false);
    setIgnore((orphaned?.ignore ?? []).map((i) => ({ group: i.group ?? '', kind: i.kind ?? '', name: i.name ?? '' })));
    setEditing(true);
  };
  const errors = ignore.map((r) => (!(r.kind ?? '').trim() && !(r.name ?? '').trim() && !(r.group ?? '').trim() ? 'Fill in at least one of group, kind or name.' : null));

  const submit = () => {
    if (!form.check(errors.flatMap((e, i) => (e ? [{ id: `orphan-${i}-group`, message: `Ignore rule ${i + 1}: ${e}` }] : [])))) return;
    saver.save(
      (s) => {
        const copy = { ...s };
        if (!on) {
          delete copy.orphanedResources;
          return copy;
        }
        const rules = ignore.map((r) => {
          const rule: { group?: string; kind?: string; name?: string } = {};
          if ((r.group ?? '').trim()) rule.group = (r.group ?? '').trim();
          if ((r.kind ?? '').trim()) rule.kind = (r.kind ?? '').trim();
          if ((r.name ?? '').trim()) rule.name = (r.name ?? '').trim();
          return rule;
        });
        copy.orphanedResources = { warn, ...(rules.length ? { ignore: rules } : {}) };
        return copy;
      },
      'Orphaned resource monitoring updated',
      () => setEditing(false),
    );
  };

  return (
    <div className={styles.stack}>
      <ProjectSection
        id="project-orphaned"
        title="Orphaned resources monitoring"
        description="Finds resources in the project's destination namespaces that no application manages."
        onEdit={open}
        editLabel="Edit orphaned resources monitoring"
      >
        <KeyValueList
          items={[
            { key: 'enabled', label: 'Monitoring', value: enabled ? 'Enabled' : 'Disabled' },
            {
              key: 'warn',
              label: 'Warning condition',
              value: enabled ? (orphaned.warn ? 'Applications show an OrphanedResourceWarning' : 'Orphans are listed without a warning') : undefined,
            },
          ]}
        />
        {enabled && (orphaned.ignore ?? []).length > 0 ? (
          <Table
            caption="Ignored orphaned resources"
            rows={orphaned.ignore ?? []}
            getRowKey={(r) => `${r.group ?? ''}/${r.kind ?? ''}/${r.name ?? ''}`}
            columns={[
              { id: 'group', header: 'Group', cell: (r) => <Mono>{orDefault(r.group, 'any')}</Mono> },
              { id: 'kind', header: 'Kind', cell: (r) => <Mono>{orDefault(r.kind, 'any')}</Mono> },
              { id: 'name', header: 'Name', cell: (r) => <Mono>{orDefault(r.name, 'any')}</Mono> },
            ]}
          />
        ) : enabled ? (
          <p className={styles.muted}>No ignore rules.</p>
        ) : null}
      </ProjectSection>
      {editing ? (
        <SectionDrawer
          title="Edit orphaned resources monitoring"
          onClose={() => setEditing(false)}
          onSubmit={submit}
          pending={saver.pending}
          error={saver.error}
          onReload={ctx.reload}
          errors={form.errors}
          attempt={form.attempt}
        >
          <Switch checked={on} onChange={setOn} label="Monitor orphaned resources" />
          {on ? (
            <>
              <Checkbox
                label="Warn on applications"
                description="Raise an OrphanedResourceWarning condition on applications whose namespaces contain orphans."
                checked={warn}
                onChange={(e) => setWarn(e.target.checked)}
              />
              <Suggestions id="orphan-groups" values={groupSuggestions(true)} />
              <Suggestions id="orphan-kinds" values={kindSuggestions(true)} />
              <RowListEditor
                legend="Ignore rules"
                noun="ignore rule"
                idPrefix="orphan"
                rows={ignore}
                onChange={setIgnore}
                fields={[
                  { key: 'group', label: 'Group', mono: true, list: 'orphan-groups', placeholder: 'any' },
                  { key: 'kind', label: 'Kind', mono: true, list: 'orphan-kinds', placeholder: 'ConfigMap' },
                  { key: 'name', label: 'Name', mono: true, placeholder: 'kube-root-ca.crt' },
                ]}
                errors={form.submitted ? errors : undefined}
                help="Resources matching a rule are not reported. Empty fields match anything; names accept globs."
                emptyText="No ignore rules."
              />
            </>
          ) : null}
        </SectionDrawer>
      ) : null}
    </div>
  );
}
