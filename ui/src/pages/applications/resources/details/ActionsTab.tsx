import { useState } from 'react';
import { useResourceActions, useRunResourceAction } from '../../../../api/queries';
import type { Application, ResourceAction } from '../../../../api/types';
import { Badge, Button, Dialog, EmptyState, FormField, Input, Skeleton, useToast } from '../../../../design';
import { ErrorBanner } from '../../../../shell/errors';
import { identifierOf, nonEmpty, type ResourceItem } from '../model/resources';
import styles from './Details.module.css';

const DESTRUCTIVE = /delete|terminate|abort|kill|stop|restart|evict|drain|pause|suspend|scale/i;

export function isDestructiveAction(name: string): boolean {
  return DESTRUCTIVE.test(name);
}

function actionTitle(action: ResourceAction): string {
  return nonEmpty(action.displayName) ?? action.name;
}

/** Resource actions (built-in or configured Lua actions), each confirmed before running. */
export function ActionsTab({ app, item }: { app: Application; item: ResourceItem }) {
  const name = app.metadata.name ?? '';
  const scope = { appNamespace: app.metadata.namespace, project: app.spec.project };
  const resource = identifierOf(item);
  const actions = useResourceActions(name, resource, scope);
  const run = useRunResourceAction();
  const toast = useToast();
  const [pending, setPending] = useState<ResourceAction | null>(null);
  const [values, setValues] = useState<Record<string, string>>({});
  const label = `${item.kind} ${item.name}`;

  if (actions.isPending) {
    return (
      <div aria-busy="true" className={styles.tabBody}>
        <span className="sr-only">Loading actions</span>
        <Skeleton height={32} />
        <Skeleton height={32} />
      </div>
    );
  }
  if (actions.isError) return <ErrorBanner error={actions.error} onRetry={() => void actions.refetch()} title="Could not load actions" />;
  const list = actions.data.actions ?? [];
  if (list.length === 0) {
    return (
      <EmptyState title="No actions for this resource" headingLevel={3}>
        <p>Actions come from built-in or configured resource customizations (Lua action scripts) for this kind. None apply to {item.kind}.</p>
      </EmptyState>
    );
  }

  const open = (action: ResourceAction) => {
    run.reset();
    // Discovery declares parameter names only (ResourceActionParam, as upstream v3.5.5): no defaults.
    setValues(Object.fromEntries((action.params ?? []).map((p) => [p.name, ''])));
    setPending(action);
  };
  const confirm = () => {
    if (!pending) return;
    const action = pending;
    run.mutate(
      {
        name,
        scope,
        resource,
        request: {
          action: action.name,
          resourceActionParameters: (action.params ?? []).map((p) => ({ name: p.name, value: values[p.name] ?? '' })),
        },
      },
      {
        onSuccess: () => {
          setPending(null);
          toast.toast({ title: `Action ${actionTitle(action)} ran on ${label}`, tone: 'success' });
        },
      },
    );
  };
  const destructive = pending ? isDestructiveAction(pending.name) : false;

  return (
    <div className={styles.tabBody}>
      <ul className={styles.actionList} aria-label={`Actions for ${label}`}>
        {list.map((action) => (
          <li key={action.name} className={styles.actionRow}>
            <span className={styles.actionName}>
              {actionTitle(action)}
              {action.params && action.params.length > 0 ? <Badge>{`${action.params.length} ${action.params.length === 1 ? 'input' : 'inputs'}`}</Badge> : null}
              {action.disabled ? <Badge>Not available now</Badge> : null}
            </span>
            <Button size="sm" variant={isDestructiveAction(action.name) ? 'danger' : 'secondary'} disabled={action.disabled} onClick={() => open(action)}>
              Run {actionTitle(action)}
            </Button>
          </li>
        ))}
      </ul>
      <Dialog
        open={pending !== null}
        onClose={() => setPending(null)}
        title={pending ? `Run ${actionTitle(pending)}?` : ''}
        description={`The action changes ${label} in the cluster right away.`}
        footer={
          <>
            <Button onClick={() => setPending(null)}>Cancel</Button>
            <Button variant={destructive ? 'danger' : 'primary'} loading={run.isPending} onClick={confirm}>
              Run {pending ? actionTitle(pending) : ''}
            </Button>
          </>
        }
      >
        <div className={styles.dialogBody}>
          {destructive ? (
            <p className={styles.warningText}>This action can interrupt the workload (for example by restarting or stopping it).</p>
          ) : null}
          {item.managed ? (
            <p className={styles.muted}>Changes not reflected in the source can be reverted by the next sync (or immediately with self-heal).</p>
          ) : null}
          {(pending?.params ?? []).map((param) => (
            <FormField key={param.name} label={param.name}>
              <Input value={values[param.name] ?? ''} onChange={(e) => setValues((v) => ({ ...v, [param.name]: e.target.value }))} />
            </FormField>
          ))}
          {run.isError ? <ErrorBanner error={run.error} title="The action failed" /> : null}
        </div>
      </Dialog>
    </div>
  );
}
