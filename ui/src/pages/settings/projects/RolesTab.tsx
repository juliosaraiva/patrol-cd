import { useId, useState, type FormEvent } from 'react';
import { useCreateProjectRoleToken, useDeleteProjectRoleToken } from '../../../api/hooks/settings';
import type { AppProject } from '../../../api/types';
import {
  Button,
  EmptyState,
  FormField,
  IconButton,
  IconClose,
  IconPlus,
  Input,
  Mono,
  Table,
  Timestamp,
  useToast,
} from '../../../design';
import { formatDuration, parseTokenLifetime } from '../common/duration';
import { nonEmpty, StringListEditor } from '../common/editors';
import { ActionError, ConfirmDialog, ErrorSummary, useFormErrors, type FieldError } from '../common/feedback';
import { TokenDialog } from '../common/TokenDialog';
import styles from '../common/common.module.css';
import { PROJECT_POLICY_RESOURCES, validateGroupName, validateProjectPolicy, validateRoleName } from '../common/trustData';
import { fromUnix, ProjectSection, SectionDrawer, useProjectSave, type ProjectContext } from './shared';

type Role = NonNullable<AppProject['spec']['roles']>[number];
interface RoleToken {
  iat: number;
  exp?: number | undefined;
  id?: string | undefined;
}

/**
 * Identity of a token, as the server keys it: its id (jti), or the issue time for legacy tokens
 * without one. Two tokens of a role may share the same iat second, so iat alone is not unique.
 */
export function tokenKey(t: RoleToken): string {
  return t.id !== undefined && t.id !== '' ? t.id : String(t.iat);
}

/**
 * Tokens of a role from status and spec (legacy), merged by {@link tokenKey} like the server's
 * NormalizeJWTTokens (spec entries win on a collision), oldest first.
 */
export function roleTokens(project: AppProject, role: string): RoleToken[] {
  const fromSpec = project.spec.roles?.find((r) => r.name === role)?.jwtTokens ?? [];
  const fromStatus = project.status?.jwtTokensByRole?.[role]?.items ?? [];
  const byKey = new Map<string, RoleToken>();
  for (const t of [...fromStatus, ...fromSpec]) byKey.set(tokenKey(t), t);
  return [...byKey.values()].sort((a, b) => a.iat - b.iat || tokenKey(a).localeCompare(tokenKey(b)));
}

function TokensPanel({ project, role }: { project: AppProject; role: string }) {
  const name = project.metadata.name ?? '';
  const formId = useId();
  const { toast } = useToast();
  const create = useCreateProjectRoleToken(name);
  const remove = useDeleteProjectRoleToken(name);
  const [id, setId] = useState('');
  const [expires, setExpires] = useState('');
  const [issued, setIssued] = useState<string | null>(null);
  const [deleting, setDeleting] = useState<RoleToken | null>(null);
  const tokens = roleTokens(project, role);
  const lifetime = parseTokenLifetime(expires);
  const now = Date.now();
  const errors: FieldError[] = [];
  if (lifetime === null) errors.push({ id: `${formId}-expires`, message: 'Use a duration such as 30m, 12h, 7d or 1w; leave empty for no expiry.' });
  if (id.trim() !== '' && !/^[A-Za-z0-9._-]+$/.test(id.trim())) errors.push({ id: `${formId}-id`, message: 'Token IDs use letters, digits, ".", "-" and "_".' });
  const form = useFormErrors(errors);
  const expired = tokens.filter((t) => t.exp !== undefined && t.exp > 0 && t.exp * 1000 < now);
  const [purging, setPurging] = useState(false);

  const deleteExpired = async () => {
    setPurging(true);
    let failed = 0;
    for (const t of expired) {
      try {
        await remove.mutateAsync({ role, issuedAt: t.iat, id: t.id });
      } catch {
        failed += 1;
      }
    }
    setPurging(false);
    toast(
      failed === 0
        ? { tone: 'success', title: `Deleted ${expired.length} expired token${expired.length === 1 ? '' : 's'}` }
        : { tone: 'error', title: `Could not delete ${failed} expired token${failed === 1 ? '' : 's'}`, description: 'Retry, or delete them one by one.' },
    );
  };

  const onCreate = (event: FormEvent) => {
    event.preventDefault();
    if (!form.check()) return;
    create.mutate(
      { role, body: { id: id.trim() || undefined, expiresIn: lifetime ?? 0 } },
      {
        onSuccess: (result) => {
          setIssued(result.token);
          setId('');
          setExpires('');
          form.reset();
        },
      },
    );
  };

  return (
    <section className={styles.section} aria-labelledby={`${formId}-title`}>
      <h3 id={`${formId}-title`}>JWT tokens</h3>
      <p className={styles.small}>Tokens authenticate automation as this role. They are independent of the role form above and take effect immediately.</p>
      {expired.length > 0 ? (
        <div>
          <Button size="sm" onClick={() => void deleteExpired()} loading={purging}>
            {`Delete ${expired.length} expired token${expired.length === 1 ? '' : 's'}`}
          </Button>
        </div>
      ) : null}
      {tokens.length === 0 ? (
        <p className={styles.muted}>No tokens issued for this role.</p>
      ) : (
        <Table
          caption={`Tokens of role ${role}`}
          className={styles.mediumTable}
          rows={tokens}
          getRowKey={tokenKey}
          columns={[
            { id: 'id', header: 'ID', cell: (t) => (t.id ? <Mono>{t.id}</Mono> : '—') },
            { id: 'iat', header: 'Issued', width: '140px', cell: (t) => <Timestamp value={fromUnix(t.iat)} /> },
            {
              id: 'exp',
              header: 'Expires',
              width: '160px',
              cell: (t) => (t.exp ? (t.exp * 1000 < now ? <span>Expired <Timestamp value={fromUnix(t.exp)} /></span> : <Timestamp value={fromUnix(t.exp)} />) : 'Never'),
            },
            {
              id: 'actions',
              header: 'Actions',
              hideHeader: true,
              width: '56px',
              align: 'end',
              cell: (t) => <IconButton aria-label={`Delete token ${tokenKey(t)}`} icon={<IconClose />} size="sm" onClick={() => setDeleting(t)} />,
            },
          ]}
        />
      )}
      <form id={formId} className={styles.form} onSubmit={onCreate} noValidate aria-label="Create token">
        <ErrorSummary errors={form.errors} focusKey={form.attempt} />
        {create.error ? <ActionError error={create.error} action="create the token" /> : null}
        <div className={styles.formGrid}>
          <FormField label="Token ID" id={`${formId}-id`} help="Optional name to recognize the token." error={form.errorFor(`${formId}-id`)}>
            <Input value={id} onChange={(e) => setId(e.target.value)} mono autoComplete="off" />
          </FormField>
          <FormField
            label="Expires in"
            id={`${formId}-expires`}
            error={form.errorFor(`${formId}-expires`)}
            help={lifetime ? `Expires ${formatDuration(lifetime * 1000)} after creation.` : 'For example 12h, 7d or 1w. Empty: never expires.'}
          >
            <Input value={expires} onChange={(e) => setExpires(e.target.value)} placeholder="7d" autoComplete="off" />
          </FormField>
        </div>
        <div>
          <Button type="submit" icon={<IconPlus />} loading={create.isPending}>
            Create token
          </Button>
        </div>
      </form>
      {issued ? <TokenDialog token={issued} title={`Token for role ${role}`} onClose={() => setIssued(null)} /> : null}
      <ConfirmDialog
        open={deleting !== null}
        onClose={() => {
          setDeleting(null);
          remove.reset();
        }}
        title="Delete token?"
        confirmLabel="Delete token"
        action="delete the token"
        busy={remove.isPending}
        error={remove.error}
        onConfirm={() => {
          if (!deleting) return;
          remove.mutate(
            { role, issuedAt: deleting.iat, id: deleting.id },
            {
              onSuccess: () => {
                toast({ tone: 'success', title: 'Token deleted', description: tokenKey(deleting) });
                setDeleting(null);
              },
            },
          );
        }}
      >
        <p>Clients using this token are rejected from now on.</p>
      </ConfirmDialog>
    </section>
  );
}

function policyTemplate(project: string, role: string): string {
  return `p, proj:${project}:${role || '<role>'}, applications, get, ${project}/*, allow`;
}

function RoleDrawer({ ctx, role, onClose }: { ctx: ProjectContext; role: Role | null; onClose: () => void }) {
  const project = ctx.project;
  const projectName = project.metadata.name ?? '';
  const creating = role === null;
  const saver = useProjectSave(ctx);
  const [name, setName] = useState(role?.name ?? '');
  const [description, setDescription] = useState(role?.description ?? '');
  const [policies, setPolicies] = useState<string[]>(role?.policies ?? []);
  const [groups, setGroups] = useState<string[]>(role?.groups ?? []);
  const roleName = name.trim();
  const policyErrors = policies.map((p) => (p.trim() === '' ? 'Enter a policy or remove the row.' : validateProjectPolicy(projectName, roleName, p)));
  const groupErrors = groups.map((g) => validateGroupName(g));
  const errors: FieldError[] = [];
  const nameError = creating ? (validateRoleName(roleName) ?? ((project.spec.roles ?? []).some((r) => r.name === roleName) ? `A role named "${roleName}" already exists.` : null)) : null;
  if (nameError) errors.push({ id: 'role-name', message: nameError });
  policyErrors.forEach((e, i) => e && errors.push({ id: `role-policy-${i}-value`, message: `Policy ${i + 1}: ${e}` }));
  groupErrors.forEach((e, i) => e && errors.push({ id: `role-group-${i}-value`, message: `Group ${i + 1}: ${e}` }));
  const form = useFormErrors(errors);

  const submit = () => {
    if (!form.check()) return;
    const edits = {
      name: roleName,
      description: description.trim() || undefined,
      policies: nonEmpty(policies),
      groups: nonEmpty(groups),
    };
    saver.save(
      // Start from the role in the latest loaded spec, never from the copy taken when the drawer
      // opened: its jwtTokens may list tokens deleted since (in the panel below), and writing
      // them back would make the server merge the revoked tokens into status again.
      (s) => ({
        ...s,
        roles: creating ? [...(s.roles ?? []), edits] : (s.roles ?? []).map((r) => (r.name === roleName ? { ...r, ...edits } : r)),
      }),
      creating ? 'Role created' : 'Role updated',
      onClose,
    );
  };

  return (
    <SectionDrawer
      title={creating ? 'Add role' : `Role ${role.name}`}
      description="Policies grant this role access to the project's objects; members get the role through their SSO groups or a JWT token."
      onClose={onClose}
      onSubmit={submit}
      pending={saver.pending}
      error={saver.error}
      onReload={ctx.reload}
      errors={form.errors}
      attempt={form.attempt}
      submitLabel={creating ? 'Create role' : 'Save role'}
      extra={creating ? null : <TokensPanel project={project} role={role.name} />}
    >
      <FormField label="Role name" id="role-name" required={creating} error={form.errorFor('role-name')} help={creating ? 'Letters, digits, "-" and "_".' : 'Role names cannot be changed.'}>
        <Input value={name} onChange={(e) => setName(e.target.value)} disabled={!creating} mono autoComplete="off" />
      </FormField>
      <FormField label="Description" id="role-description">
        <Input value={description} onChange={(e) => setDescription(e.target.value)} />
      </FormField>
      <StringListEditor
        legend="Policies"
        label="Policy"
        noun="policy"
        idPrefix="role-policy"
        values={policies}
        onChange={setPolicies}
        placeholder={policyTemplate(projectName, roleName)}
        errors={form.submitted ? policyErrors : undefined}
        help={
          <>
            Format: <Mono>{policyTemplate(projectName, roleName)}</Mono>. Resources: {PROJECT_POLICY_RESOURCES.join(', ')}. Actions: get,
            create, update, delete, sync, override, action/&lt;group/kind/name&gt; or *.
          </>
        }
        emptyText="No policies: the role grants nothing."
      />
      {policies.length === 0 || policies.every((p) => p.trim() === '') ? (
        <div>
          <Button size="sm" onClick={() => setPolicies([...policies.filter((p) => p.trim() !== ''), policyTemplate(projectName, roleName)])}>
            Insert read-only policy
          </Button>
        </div>
      ) : null}
      <StringListEditor
        legend="Groups"
        label="Group"
        noun="group"
        idPrefix="role-group"
        values={groups}
        onChange={setGroups}
        placeholder="my-org:team"
        errors={form.submitted ? groupErrors : undefined}
        help="SSO groups (OIDC groups claim) whose members get this role. Quote names that contain commas."
        emptyText="No groups."
      />
    </SectionDrawer>
  );
}

/** Roles tab: project roles with policies, groups and JWT tokens (API-105, TEN-064). */
export function RolesTab({ ctx }: { ctx: ProjectContext }) {
  const roles = ctx.project.spec.roles ?? [];
  const saver = useProjectSave(ctx);
  const [editing, setEditing] = useState<Role | null | undefined>(undefined);
  const [deleting, setDeleting] = useState<Role | null>(null);

  return (
    <div className={styles.stack}>
      <ProjectSection
        id="project-roles"
        title="Roles"
        description="Project roles scope permissions to this project's applications, repositories and clusters."
      >
        <div>
          <Button variant="primary" icon={<IconPlus />} onClick={() => setEditing(null)}>
            Add role
          </Button>
        </div>
        {roles.length === 0 ? (
          <EmptyState headingLevel={3} title="No roles">
            Add a role to give a team or CI pipeline access limited to this project, for example to sync its applications.
          </EmptyState>
        ) : (
          <Table
            caption="Project roles"
            className={styles.mediumTable}
            rows={roles}
            getRowKey={(r) => r.name}
            onRowActivate={(r) => setEditing(r)}
            columns={[
              {
                id: 'name',
                header: 'Name',
                width: '160px',
                cell: (r) => (
                  <Button variant="ghost" size="sm" onClick={() => setEditing(r)} aria-label={`Edit role ${r.name}`}>
                    {r.name}
                  </Button>
                ),
                sortValue: (r) => r.name,
              },
              { id: 'description', header: 'Description', cell: (r) => r.description ?? '—' },
              { id: 'policies', header: 'Policies', width: '90px', align: 'end', cell: (r) => r.policies?.length ?? 0 },
              { id: 'groups', header: 'Groups', width: '180px', cell: (r) => (r.groups?.length ? <span title={r.groups.join(', ')}>{r.groups.join(', ')}</span> : '—') },
              { id: 'tokens', header: 'Tokens', width: '80px', align: 'end', cell: (r) => roleTokens(ctx.project, r.name).length },
              {
                id: 'actions',
                header: 'Actions',
                hideHeader: true,
                width: '56px',
                align: 'end',
                cell: (r) => <IconButton aria-label={`Delete role ${r.name}`} icon={<IconClose />} size="sm" onClick={() => setDeleting(r)} />,
              },
            ]}
          />
        )}
      </ProjectSection>
      {editing !== undefined ? <RoleDrawer ctx={ctx} role={editing} onClose={() => setEditing(undefined)} /> : null}
      <ConfirmDialog
        open={deleting !== null}
        onClose={() => {
          setDeleting(null);
          saver.reset();
        }}
        title="Delete role?"
        confirmLabel="Delete role"
        action="delete the role"
        busy={saver.pending}
        error={saver.error}
        onConfirm={() => {
          if (!deleting) return;
          saver.save(
            (s) => ({ ...s, roles: (s.roles ?? []).filter((r) => r.name !== deleting.name) }),
            'Role deleted',
            () => setDeleting(null),
          );
        }}
      >
        <p>
          Members of the role&apos;s groups lose its permissions and its {roleTokens(ctx.project, deleting?.name ?? '').length} token(s) stop
          working.
        </p>
      </ConfirmDialog>
    </div>
  );
}
