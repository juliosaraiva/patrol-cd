import { useId, useState, type FormEvent, type ReactNode } from 'react';
import { useCreateSCMProvider, useDeleteSCMProvider, useUpdateSCMProvider } from '../../../api/hooks/settings';
import { webhookURL } from '../../../api/endpoints/misc';
import { getSCMProvider } from '../../../api/endpoints/trust';
import { useSCMProviders, useSettings } from '../../../api/queries/resources';
import type { SCMProvider } from '../../../api/types';
import {
  Badge,
  Banner,
  Button,
  Checkbox,
  CodeBlock,
  ConnectionStatus,
  CopyButton,
  Drawer,
  EmptyState,
  FormField,
  IconPlus,
  Input,
  KeyValueList,
  Mono,
  PageHeader,
  RadioGroup,
  Select,
  Switch,
  Table,
  Timestamp,
  useToast,
  type Column,
} from '../../../design';
import { useDocumentTitle } from '../../../lib/useDocumentTitle';
import { ErrorBanner } from '../../../shell/errors';
import { ActionError, ConfirmDialog, ErrorSummary, LoadingBlock, useFormErrors } from '../common/feedback';
import styles from '../common/common.module.css';
import {
  authSummary,
  emptyProviderForm,
  formFromProvider,
  PROVIDER_TYPES,
  providerFieldId,
  readiness,
  toProviderRequest,
  typeLabel,
  validateProviderForm,
  webhookProviderName,
  type AuthMethod,
  type ProviderForm,
  type SecretField,
} from './model';

type TextKey = { [K in keyof ProviderForm]: ProviderForm[K] extends string ? K : never }[keyof ProviderForm];

/**
 * Reference to a key of an existing Secret. SCM provider writes carry only secretRefs: the
 * Secret is created outside Patrol and its value never passes through the UI or the API.
 */
function SecretRefFields({
  prefix,
  label,
  field,
  onChange,
  errorFor,
  namespace,
}: {
  prefix: string;
  label: string;
  field: SecretField;
  onChange: (f: SecretField) => void;
  errorFor: (id: string) => string | undefined;
  namespace: string;
}) {
  const id = (k: string) => providerFieldId(`${prefix}-${k}`);
  return (
    <fieldset className={styles.fieldset}>
      <legend className={styles.legend}>{label}</legend>
      <p className={styles.small}>
        Reference a key of an existing Secret in {namespace}. Create the Secret with kubectl, GitOps or an external secrets operator; Patrol
        never reads or stores its value through the UI.
      </p>
      <div className={styles.formGrid}>
        <FormField label="Secret name" id={id('name')} required error={errorFor(id('name'))}>
          <Input value={field.name} onChange={(e) => onChange({ ...field, name: e.target.value })} mono autoComplete="off" />
        </FormField>
        <FormField label="Key" id={id('key')} required error={errorFor(id('key'))}>
          <Input value={field.key} onChange={(e) => onChange({ ...field, key: e.target.value })} mono autoComplete="off" />
        </FormField>
      </div>
    </fieldset>
  );
}

function ProviderDrawer({ existing: initial, onClose }: { existing?: SCMProvider | undefined; onClose: () => void }) {
  /** Replaced by the latest version after a 409 conflict and reload. */
  const [existing, setExisting] = useState(initial);
  const formId = useId();
  const { toast } = useToast();
  const settings = useSettings();
  const namespace = settings.data?.controllerNamespace ?? 'the control-plane namespace';
  const editing = existing !== undefined;
  const create = useCreateSCMProvider();
  const update = useUpdateSCMProvider();
  const [f, setF] = useState<ProviderForm>(() => (existing ? formFromProvider(existing) : emptyProviderForm()));
  const form = useFormErrors(validateProviderForm(f, editing));
  const mutation = editing ? update : create;
  const type = PROVIDER_TYPES.find((t) => t.value === f.type);
  const errorFor = (id: string) => form.errorFor(id);
  const fid = providerFieldId;

  const onSubmit = (event: FormEvent) => {
    event.preventDefault();
    if (!form.check()) return;
    const body = toProviderRequest(f, existing);
    const done = () => {
      toast({ tone: 'success', title: editing ? 'SCM provider updated' : 'SCM provider created', description: f.name.trim() });
      onClose();
    };
    if (existing) update.mutate({ name: existing.metadata.name ?? '', body }, { onSuccess: done });
    else create.mutate(body, { onSuccess: done });
  };

  const authOptions: { value: AuthMethod; label: string; description: string }[] = [
    { value: 'none', label: 'Anonymous', description: 'Public data only; strict rate limits apply.' },
    { value: 'token', label: 'Access token', description: 'Personal, project or service access token.' },
    { value: 'basic', label: 'Username and password', description: 'Secret with keys "username" and "password" (Bitbucket app passwords).' },
    ...(f.type === 'github' ? [{ value: 'githubApp' as const, label: 'GitHub App', description: 'Short-lived installation tokens; required for check runs.' }] : []),
  ];

  const text = (key: TextKey, label: ReactNode, opts: { help?: ReactNode; required?: boolean; mono?: boolean; placeholder?: string; disabled?: boolean } = {}) => (
    <FormField label={label} id={fid(key)} required={opts.required} error={errorFor(fid(key))} help={opts.help}>
      <Input
        value={f[key]}
        onChange={(e) => setF({ ...f, [key]: e.target.value })}
        mono={opts.mono}
        placeholder={opts.placeholder}
        disabled={opts.disabled}
        autoComplete="off"
        spellCheck={false}
      />
    </FormField>
  );

  return (
    <Drawer
      open
      onClose={onClose}
      width="720px"
      title={editing ? `Edit ${existing.metadata.name ?? ''}` : 'New SCM provider'}
      description="Connection used by ApplicationSet generators, webhooks and commit status reporting. Credentials are referenced from Kubernetes Secrets."
      footer={
        <>
          <Button onClick={onClose}>Cancel</Button>
          <Button variant="primary" type="submit" form={formId} loading={mutation.isPending}>
            {editing ? 'Save' : 'Create provider'}
          </Button>
        </>
      }
    >
      <form id={formId} className={styles.form} onSubmit={onSubmit} noValidate aria-label={editing ? 'Edit SCM provider' : 'New SCM provider'}>
        <ErrorSummary errors={form.errors} focusKey={form.attempt} />
        {mutation.error ? (
          <ActionError
            error={mutation.error}
            action={editing ? 'update the SCM provider' : 'create the SCM provider'}
            onReload={
              existing
                ? () =>
                    void getSCMProvider(existing.metadata.name ?? '').then(
                      (fresh) => {
                        setExisting(fresh);
                        update.reset();
                        toast({ tone: 'info', title: 'Loaded the latest version', description: 'Review your changes and save again.' });
                      },
                      (error: unknown) => toast({ tone: 'error', title: 'Could not reload the provider', description: error instanceof Error ? error.message : undefined }),
                    )
                : undefined
            }
          />
        ) : null}
        <div className={styles.formGrid}>
          {text('name', 'Name', { required: !editing, mono: true, disabled: editing, help: editing ? 'Names cannot be changed.' : 'Referenced by repositories and generators.' })}
          <FormField label="Type" id={fid('type')} required>
            <Select
              value={f.type}
              onChange={(e) => {
                const next = e.target.value as ProviderForm['type'];
                setF({ ...f, type: next, auth: f.auth === 'githubApp' && next !== 'github' ? 'token' : f.auth });
              }}
              options={PROVIDER_TYPES.map((t) => ({ value: t.value, label: t.label }))}
            />
          </FormField>
          {text('apiURL', 'API URL', {
            mono: true,
            required: type?.selfHosted,
            placeholder: type?.defaultAPI ?? 'https://scm.example.com/api',
            help: type?.selfHosted ? 'Required for self-hosted servers.' : `Leave empty for ${type?.defaultAPI ?? 'the public service'}; set it for self-hosted servers.`,
          })}
          {text('webURL', 'Web URL', { mono: true, placeholder: 'https://scm.example.com', help: 'Used for links in the UI. Optional.' })}
        </div>
        <Checkbox label="Skip TLS verification" description="Only for testing against servers with untrusted certificates." checked={f.insecure} onChange={(e) => setF({ ...f, insecure: e.target.checked })} />
        <Checkbox label="Custom CA certificate" description="Trust a private CA for the API URL." checked={f.caEnabled} onChange={(e) => setF({ ...f, caEnabled: e.target.checked })} />
        {f.caEnabled ? <SecretRefFields prefix="ca" label="CA certificate" field={f.ca} onChange={(ca) => setF({ ...f, ca })} errorFor={errorFor} namespace={namespace} /> : null}

        <RadioGroup legend="Authentication" value={f.auth} onChange={(auth) => setF({ ...f, auth })} options={authOptions} />
        {f.auth === 'token' ? <SecretRefFields prefix="token" label="Token" field={f.token} onChange={(token) => setF({ ...f, token })} errorFor={errorFor} namespace={namespace} /> : null}
        {f.auth === 'basic' ? (
          <fieldset className={styles.fieldset}>
            <legend className={styles.legend}>Username and password</legend>
            {text('basicSecret', 'Secret name', { mono: true, required: true, help: `An existing Secret in ${namespace} with the keys "username" and "password".` })}
          </fieldset>
        ) : null}
        {f.auth === 'githubApp' ? (
          <>
            <div className={styles.formGrid}>
              {text('appID', 'App ID', { required: true, placeholder: '123456' })}
              {text('installationID', 'Installation ID', { placeholder: 'Discovered automatically', help: 'Optional; looked up from the repository owner when empty.' })}
            </div>
            <SecretRefFields prefix="appKey" label="Private key" field={f.appKey} onChange={(appKey) => setF({ ...f, appKey })} errorFor={errorFor} namespace={namespace} />
          </>
        ) : null}

        <Switch checked={f.webhookEnabled} onChange={(webhookEnabled) => setF({ ...f, webhookEnabled })} label="Verify webhook deliveries" />
        {f.webhookEnabled ? (
          <SecretRefFields prefix="webhook" label="Webhook secret" field={f.webhook} onChange={(webhook) => setF({ ...f, webhook })} errorFor={errorFor} namespace={namespace} />
        ) : null}

        <Switch checked={f.statusEnabled} onChange={(statusEnabled) => setF({ ...f, statusEnabled })} label="Report sync status to commits" />
        {f.statusEnabled ? (
          <div className={styles.formGrid}>
            <FormField label="Reporting mode" id={fid('statusMode')} error={errorFor(fid('statusMode'))}>
              <Select
                value={f.statusMode}
                onChange={(e) => setF({ ...f, statusMode: e.target.value as ProviderForm['statusMode'] })}
                options={[
                  { value: 'statuses', label: 'Commit statuses' },
                  { value: 'checks', label: 'Check runs (GitHub App only)' },
                ]}
              />
            </FormField>
            {text('contextPrefix', 'Context prefix', { mono: true, placeholder: 'patrol', help: 'Prefix of the status context, e.g. patrol/<application>.' })}
          </div>
        ) : null}

        <Switch checked={f.rateLimitEnabled} onChange={(rateLimitEnabled) => setF({ ...f, rateLimitEnabled })} label="Limit request rate" />
        {f.rateLimitEnabled ? (
          <div className={styles.formGrid}>
            <FormField label="Requests per second" id={fid('rps')} required error={errorFor(fid('rps'))}>
              <Input value={f.requestsPerSecond} onChange={(e) => setF({ ...f, requestsPerSecond: e.target.value })} inputMode="decimal" />
            </FormField>
            {text('burst', 'Burst', { help: 'Requests allowed at once above the steady rate.' })}
          </div>
        ) : null}
      </form>
    </Drawer>
  );
}

function ProviderDetails({ provider, onClose, onEdit, onDelete }: { provider: SCMProvider; onClose: () => void; onEdit: () => void; onDelete: () => void }) {
  const ready = readiness(provider);
  const status = provider.status;
  const name = provider.metadata.name ?? '';
  // The receiver is addressed by provider type, not by the SCMProvider object's name.
  const hookProvider = webhookProviderName(provider.spec.type);
  const hook = hookProvider ? webhookURL(hookProvider) : undefined;
  return (
    <Drawer
      open
      onClose={onClose}
      width="640px"
      title={name}
      description={`${typeLabel(provider.spec.type)} provider`}
      footer={
        <>
          <Button variant="danger" onClick={onDelete}>
            Delete
          </Button>
          <Button variant="primary" onClick={onEdit}>
            Edit
          </Button>
        </>
      }
    >
      <div className={styles.stack}>
        <KeyValueList
          items={[
            { key: 'ready', label: 'Connection', value: <ConnectionStatus status={ready.status} /> },
            { key: 'message', label: 'Details', value: ready.message },
            { key: 'checked', label: 'Last checked', value: status?.lastCheckedAt ? <Timestamp value={status.lastCheckedAt} /> : undefined },
            { key: 'api', label: 'API URL', value: provider.spec.apiURL ? <Mono>{provider.spec.apiURL}</Mono> : 'Default' },
            { key: 'auth', label: 'Authentication', value: authSummary(provider) },
            {
              key: 'status',
              label: 'Status reporting',
              value: provider.spec.statusReporting?.enabled ? `${provider.spec.statusReporting.mode === 'checks' ? 'Check runs' : 'Commit statuses'}` : 'Off',
            },
            {
              key: 'rate',
              label: 'API rate limit',
              value: status?.rateLimit?.limit ? (
                <>
                  {status.rateLimit.remaining ?? 0} of {status.rateLimit.limit} left
                  {status.rateLimit.resetAt ? (
                    <>
                      , resets <Timestamp value={status.rateLimit.resetAt} />
                    </>
                  ) : null}
                </>
              ) : (
                'Not reported'
              ),
            },
            {
              key: 'capabilities',
              label: 'Capabilities',
              value: status?.capabilities?.length ? (
                <ul className={styles.tagList} aria-label="Capabilities">
                  {status.capabilities.map((c) => (
                    <li key={c}>
                      <Badge>{c}</Badge>
                    </li>
                  ))}
                </ul>
              ) : (
                'Not reported yet'
              ),
            },
          ]}
        />
        {status?.rateLimit?.limit && (status.rateLimit.remaining ?? 0) < status.rateLimit.limit * 0.1 ? (
          <Banner tone="warning" title="Rate limit nearly exhausted">
            Generators and status reporting slow down until the limit resets. Consider GitHub App authentication or a lower request rate.
          </Banner>
        ) : null}
        {provider.spec.webhook ? (
          <section className={styles.section} aria-labelledby="scm-webhook">
            <h3 id="scm-webhook">Webhook</h3>
            {hook ? (
              <>
                <p className={styles.muted}>
                  Register this URL in {typeLabel(provider.spec.type)} with the secret stored in <Mono>{provider.spec.webhook.secretRef.name}</Mono>{' '}
                  (key <Mono>{provider.spec.webhook.secretRef.key}</Mono>).
                </p>
                <div className={styles.actions}>
                  <Mono>{hook}</Mono>
                  <CopyButton value={hook} label="Copy webhook URL" />
                </div>
              </>
            ) : (
              <p className={styles.muted}>Patrol has no webhook receiver for {typeLabel(provider.spec.type)}; changes are picked up by polling.</p>
            )}
          </section>
        ) : null}
        {(status?.conditions?.length ?? 0) > 0 ? (
          <CodeBlock
            title="Conditions"
            copyable={false}
            wrap
            code={(status?.conditions ?? []).map((c) => `${c.type}=${c.status} ${c.reason}: ${c.message}`).join('\n')}
          />
        ) : null}
      </div>
    </Drawer>
  );
}

/** /settings/scmproviders — SCM provider connections (Patrol addition). */
export function Component() {
  useDocumentTitle('SCM providers', 'Settings');
  const { toast } = useToast();
  const providers = useSCMProviders();
  const remove = useDeleteSCMProvider();
  const [creating, setCreating] = useState(false);
  const [viewing, setViewing] = useState<SCMProvider | undefined>();
  const [editing, setEditing] = useState<SCMProvider | undefined>();
  const [deleting, setDeleting] = useState<SCMProvider | undefined>();
  const items = providers.data?.items ?? [];

  const columns: Column<SCMProvider>[] = [
    {
      id: 'name',
      header: 'Name',
      width: '160px',
      cell: (p) => (
        <Button variant="ghost" size="sm" onClick={() => setViewing(p)} aria-label={`Details of ${p.metadata.name ?? ''}`}>
          {p.metadata.name}
        </Button>
      ),
      sortValue: (p) => p.metadata.name,
    },
    { id: 'type', header: 'Type', width: '140px', cell: (p) => typeLabel(p.spec.type), sortValue: (p) => typeLabel(p.spec.type) },
    { id: 'api', header: 'API URL', cell: (p) => (p.spec.apiURL ? <Mono title={p.spec.apiURL}>{p.spec.apiURL}</Mono> : 'Default') },
    { id: 'auth', header: 'Authentication', width: '190px', cell: (p) => <span title={authSummary(p)}>{authSummary(p)}</span> },
    {
      id: 'ready',
      header: 'Connection',
      width: '130px',
      cell: (p) => {
        const r = readiness(p);
        return <ConnectionStatus status={r.status} description={r.message} />;
      },
      sortValue: (p) => readiness(p).status,
    },
    {
      id: 'reporting',
      header: 'Status reporting',
      width: '140px',
      cell: (p) => (p.spec.statusReporting?.enabled ? (p.spec.statusReporting.mode === 'checks' ? 'Check runs' : 'Commit statuses') : 'Off'),
    },
    {
      id: 'rate',
      header: 'Rate limit left',
      width: '130px',
      align: 'end',
      cell: (p) => (p.status?.rateLimit?.limit ? `${p.status.rateLimit.remaining ?? 0} / ${p.status.rateLimit.limit}` : '—'),
    },
  ];

  let content;
  if (providers.isPending) content = <LoadingBlock label="Loading SCM providers" />;
  else if (providers.isError) content = <ErrorBanner error={providers.error} onRetry={() => void providers.refetch()} />;
  else if (items.length === 0) {
    content = (
      <EmptyState
        title="No SCM providers"
        action={
          <Button variant="primary" icon={<IconPlus />} onClick={() => setCreating(true)}>
            New SCM provider
          </Button>
        }
      >
        Add a provider to discover repositories and pull requests in ApplicationSet generators, verify webhooks and report sync status on
        commits. Credentials stay in Kubernetes Secrets.
      </EmptyState>
    );
  } else {
    content = (
      <Table
        caption="SCM providers"
        className={styles.extraWideTable}
        columns={columns}
        rows={items}
        getRowKey={(p) => p.metadata.name ?? ''}
        defaultSort={{ columnId: 'name', direction: 'ascending' }}
        onRowActivate={(p) => setViewing(p)}
      />
    );
  }

  return (
    <>
      <PageHeader
        title="SCM providers"
        description="Source control providers used by ApplicationSet generators, webhooks and commit status reporting."
        actions={
          items.length > 0 ? (
            <Button variant="primary" icon={<IconPlus />} onClick={() => setCreating(true)}>
              New SCM provider
            </Button>
          ) : undefined
        }
      />
      <div className={styles.stack}>{content}</div>
      {creating ? <ProviderDrawer onClose={() => setCreating(false)} /> : null}
      {editing ? <ProviderDrawer existing={editing} onClose={() => setEditing(undefined)} /> : null}
      {viewing && !editing ? (
        <ProviderDetails
          provider={viewing}
          onClose={() => setViewing(undefined)}
          onEdit={() => {
            setEditing(viewing);
            setViewing(undefined);
          }}
          onDelete={() => {
            setDeleting(viewing);
            setViewing(undefined);
          }}
        />
      ) : null}
      <ConfirmDialog
        open={deleting !== undefined}
        onClose={() => {
          setDeleting(undefined);
          remove.reset();
        }}
        title="Delete SCM provider?"
        confirmLabel="Delete provider"
        action="delete the SCM provider"
        confirmText={deleting?.metadata.name}
        busy={remove.isPending}
        error={remove.error}
        onConfirm={() => {
          const name = deleting?.metadata.name;
          if (!name) return;
          remove.mutate(name, {
            onSuccess: () => {
              toast({ tone: 'success', title: 'SCM provider deleted', description: name });
              setDeleting(undefined);
            },
          });
        }}
      >
        <p>
          Generators, repositories and webhooks that reference <Mono>{deleting?.metadata.name}</Mono> stop working. The referenced Secrets
          are not deleted.
        </p>
      </ConfirmDialog>
    </>
  );
}
