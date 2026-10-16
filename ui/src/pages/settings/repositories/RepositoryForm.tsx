import { useEffect, useId, useRef, useState, type FormEvent, type ReactNode } from 'react';
import { Link } from 'react-router';
import {
  useCreateRepository,
  useCreateRepositoryCredential,
  useUpdateRepository,
  useValidateRepository,
} from '../../../api/hooks/settings';
import { getRepository } from '../../../api/endpoints/repositories';
import { useProjects, useSCMProviders } from '../../../api/queries/resources';
import type { ConnectionState, RepositoryInfo } from '../../../api/types';
import {
  Banner,
  Button,
  Checkbox,
  Drawer,
  FormField,
  Input,
  Mono,
  RadioGroup,
  Select,
  Tabs,
  TextArea,
  useToast,
} from '../../../design';
import { ActionError, ErrorSummary, useFormErrors } from '../common/feedback';
import styles from '../common/common.module.css';
import {
  connectionGuidance,
  emptyForm,
  fieldId,
  formFromRepository,
  METHOD_LABELS,
  READ_METHODS,
  suggestedPrefix,
  toCredentialRequest,
  toRepositoryRequest,
  validateRepoForm,
  WRITE_METHODS,
  type ConnectionMethod,
  type FormMode,
  type RepoFormState,
} from './model';

export interface RepositoryFormProps {
  open: boolean;
  onClose: () => void;
  /** `repository`: connect or edit a repository; `template`: create a credential template. */
  mode: FormMode;
  purpose: 'read' | 'write';
  /** Repository being edited (repository mode only). */
  existing?: RepositoryInfo | undefined;
}

type BooleanKey = { [K in keyof RepoFormState]: RepoFormState[K] extends boolean ? K : never }[keyof RepoFormState];
type StringKey = { [K in keyof RepoFormState]: RepoFormState[K] extends string ? K : never }[keyof RepoFormState];

/**
 * Collapsible group of optional fields. A submit attempt that finds an error inside expands it, so
 * the inline message is visible and the error summary link can focus the field (browsers cannot
 * focus controls inside a closed <details>). It never collapses on its own.
 */
function OptionalFields({ summary, invalid, attempt, children }: { summary: string; invalid: boolean; attempt: number; children: ReactNode }) {
  const ref = useRef<HTMLDetailsElement>(null);
  useEffect(() => {
    if (invalid && ref.current) ref.current.open = true;
  }, [invalid, attempt]);
  return (
    <details ref={ref} className={styles.details}>
      <summary>{summary}</summary>
      {children}
    </details>
  );
}

const NETWORK_FIELDS: readonly (keyof RepoFormState)[] = ['proxy', 'noProxy', 'depth'];
const TLS_FIELDS: readonly (keyof RepoFormState)[] = ['tlsClientCertData', 'tlsClientCertKey'];

interface ConnectionResult {
  state: ConnectionState;
  /** The failure happened while saving: offer to save anyway. */
  fromSave: boolean;
}

/**
 * Connect / edit panel for repositories and credential templates. Repositories are tested with
 * POST …/validate before they are saved; a failed test explains the likely fix and lets the user
 * save anyway. Secret fields are write-only: when editing, empty secret fields keep the stored
 * values.
 */
export function RepositoryForm({ open, onClose, mode, purpose, existing: initial }: RepositoryFormProps) {
  /** The edited object; replaced by the latest version after a 409 conflict and reload. */
  const [existing, setExisting] = useState(initial);
  const formId = useId();
  const { toast } = useToast();
  const editing = existing !== undefined;
  const [state, setState] = useState<RepoFormState>(() => (existing ? formFromRepository(existing) : emptyForm()));
  const form = useFormErrors(validateRepoForm(state, { editing, mode }));
  const [result, setResult] = useState<ConnectionResult | null>(null);
  const [saveError, setSaveError] = useState<{ error: unknown; action: string } | null>(null);

  const createRepo = useCreateRepository(purpose);
  const updateRepo = useUpdateRepository(purpose);
  const validate = useValidateRepository(purpose);
  const createCreds = useCreateRepositoryCredential(purpose);
  const projects = useProjects();
  const providers = useSCMProviders();

  const resetForm = form.reset;
  useEffect(() => {
    if (!open) return;
    setExisting(initial);
    setState(initial ? formFromRepository(initial) : emptyForm());
    setResult(null);
    setSaveError(null);
    resetForm();
    // Reinitialize only when the panel opens for another object.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open, initial]);

  const set = <K extends keyof RepoFormState>(key: K, value: RepoFormState[K]) => {
    setState((s) => ({ ...s, [key]: value }));
    setResult(null);
  };

  const busy = createRepo.isPending || updateRepo.isPending || validate.isPending || createCreds.isPending;
  const methods = purpose === 'write' ? WRITE_METHODS : READ_METHODS;
  const kind = purpose === 'write' ? 'write repository' : 'repository';

  const check = () => form.check();

  const save = async () => {
    setSaveError(null);
    const body = toRepositoryRequest(state, purpose, existing);
    try {
      if (existing) {
        await updateRepo.mutateAsync({ url: existing.spec.url, body, appProject: existing.spec.project });
        toast({ tone: 'success', title: purpose === 'write' ? 'Write repository updated' : 'Repository updated', description: state.url });
      } else {
        await createRepo.mutateAsync({ body });
        toast({ tone: 'success', title: purpose === 'write' ? 'Write repository connected' : 'Repository connected', description: state.url });
      }
      onClose();
    } catch (error) {
      setSaveError({ error, action: editing ? `update the ${kind}` : `connect the ${kind}` });
    }
  };

  const test = async (fromSave: boolean): Promise<boolean> => {
    setSaveError(null);
    try {
      const state_ = await validate.mutateAsync({ url: state.url.trim(), body: toRepositoryRequest(state, purpose, existing) });
      setResult({ state: state_, fromSave });
      return state_.status === 'Successful';
    } catch (error) {
      setSaveError({ error, action: 'test the connection' });
      return false;
    }
  };

  const onSubmit = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (!check()) return;
    if (mode === 'template') {
      await saveTemplate();
      return;
    }
    if (await test(true)) await save();
  };

  const saveTemplate = async () => {
    setSaveError(null);
    const prefixState = mode === 'template' ? state : { ...state, url: suggestedPrefix(state.url) };
    try {
      await createCreds.mutateAsync({ body: toCredentialRequest(prefixState, purpose) });
      toast({
        tone: 'success',
        title: purpose === 'write' ? 'Write credential template saved' : 'Credential template saved',
        description: `Used for repositories under ${prefixState.url.trim()}`,
      });
      onClose();
    } catch (error) {
      setSaveError({ error, action: 'save the credential template' });
    }
  };

  /** After a 409: load the latest version and reapply the user's non-secret edits on top. */
  const reloadLatest = async () => {
    if (!existing) return;
    try {
      const fresh = await getRepository(existing.spec.url, { appProject: existing.spec.project }, { purpose });
      setExisting(fresh);
      setSaveError(null);
      toast({ tone: 'info', title: 'Loaded the latest version', description: 'Review your changes and save again.' });
    } catch (error) {
      setSaveError({ error, action: 'reload the repository' });
    }
  };

  const errorFor = (key: keyof RepoFormState) => form.errorFor(fieldId(key));
  // RepositoryInfo tells whether credentials are stored (never their values).
  const secretHelp = !editing
    ? undefined
    : existing?.hasCredentials
      ? 'Leave empty to keep the stored value.'
      : existing?.inheritedCredentials
        ? 'None stored: a matching credential template applies. Enter a value to store credentials for this repository.'
        : 'No credentials stored. Leave empty to keep it that way.';

  const text = (key: StringKey, label: ReactNode, opts: { help?: ReactNode; required?: boolean; mono?: boolean; placeholder?: string; type?: string; disabled?: boolean } = {}) => (
    <FormField label={label} id={fieldId(key)} error={errorFor(key)} help={opts.help} required={opts.required}>
      <Input
        value={state[key]}
        onChange={(e) => set(key, e.target.value)}
        mono={opts.mono}
        placeholder={opts.placeholder}
        type={opts.type}
        disabled={opts.disabled}
        autoComplete={opts.type === 'password' ? 'new-password' : 'off'}
        spellCheck={false}
      />
    </FormField>
  );
  const area = (key: StringKey, label: ReactNode, opts: { help?: ReactNode; required?: boolean; placeholder?: string } = {}) => (
    <FormField label={label} id={fieldId(key)} error={errorFor(key)} help={opts.help ?? secretHelp} required={opts.required}>
      <TextArea value={state[key]} onChange={(e) => set(key, e.target.value)} mono rows={5} placeholder={opts.placeholder} spellCheck={false} autoComplete="off" />
    </FormField>
  );
  const check_ = (key: BooleanKey, label: ReactNode, description?: ReactNode) => (
    <Checkbox id={fieldId(key)} label={label} description={description} checked={state[key]} onChange={(e) => set(key, e.target.checked)} />
  );

  const repoOnly = mode === 'repository';
  const projectOptions = [
    { value: '', label: 'No project (available to all projects)' },
    ...(projects.data?.items ?? []).map((p) => ({ value: p.metadata.name ?? '', label: p.metadata.name ?? '' })),
  ];
  const githubProviders = (providers.data?.items ?? []).filter((p) => p.spec.type === 'github');

  const identity = (urlLabel: string, urlPlaceholder: string, nameRequired = false) => (
    <>
      {repoOnly ? (
        <div className={styles.formGrid}>
          {text('name', 'Name', {
            required: nameRequired,
            help: nameRequired ? 'Required for Helm: applications refer to the chart repository by this name.' : 'Optional display name.',
          })}
          <FormField
            label="Project"
            id={fieldId('project')}
            help="Scope the repository to one project, or leave it available to every project whose source repositories allow it."
          >
            <Select value={state.project} onChange={(e) => set('project', e.target.value)} options={projectOptions} disabled={editing} />
          </FormField>
        </div>
      ) : null}
      {mode === 'template'
        ? text('url', 'URL prefix', {
            required: true,
            mono: true,
            placeholder: urlPlaceholder.replace(/\/[^/]*$/, ''),
            help: 'Every repository whose URL starts with this prefix uses these credentials unless it has its own.',
          })
        : text('url', urlLabel, { required: true, mono: true, placeholder: urlPlaceholder, disabled: editing })}
    </>
  );

  const invalidAmong = (keys: readonly (keyof RepoFormState)[]) => keys.some((key) => errorFor(key) !== undefined);

  const network = (withDepth: boolean) => (
    <OptionalFields summary="Network and clone options" invalid={invalidAmong(NETWORK_FIELDS)} attempt={form.attempt}>
      <div className={styles.formGrid}>
        {text('proxy', 'Proxy', { mono: true, placeholder: 'http://proxy.example.com:3128' })}
        {text('noProxy', 'No proxy', { mono: true, placeholder: '.internal.example.com,10.0.0.0/8', help: 'Comma-separated hosts that bypass the proxy.' })}
        {withDepth && repoOnly ? text('depth', 'Clone depth', { help: 'Shallow clone depth; leave empty for a full clone.' }) : null}
      </div>
    </OptionalFields>
  );

  const tls = (
    <OptionalFields summary="TLS client certificate" invalid={invalidAmong(TLS_FIELDS)} attempt={form.attempt}>
      <div className={styles.stackTight}>
        {area('tlsClientCertData', 'Client certificate (PEM)')}
        {area('tlsClientCertKey', 'Client certificate key (PEM)')}
      </div>
    </OptionalFields>
  );

  const basicAuth = (allowBearer: boolean) =>
    state.useAzureWorkloadIdentity ? null : (
      <>
        <RadioGroup
          legend="Authentication"
          orientation="horizontal"
          value={state.httpsAuth}
          onChange={(v) => set('httpsAuth', v)}
          options={[
            { value: 'none', label: 'None' },
            { value: 'basic', label: 'Username and password' },
            ...(allowBearer ? [{ value: 'bearer' as const, label: 'Bearer token' }] : []),
          ]}
        />
        {state.httpsAuth === 'basic' ? (
          <div className={styles.formGrid}>
            {text('username', 'Username')}
            {text('password', 'Password or access token', { type: 'password', help: secretHelp })}
          </div>
        ) : null}
        {state.httpsAuth === 'bearer'
          ? text('bearerToken', 'Bearer token', { type: 'password', required: !editing, help: secretHelp ?? 'Sent as an Authorization: Bearer header (Bitbucket Data Center access tokens).' })
          : null}
      </>
    );

  const skipVerify = repoOnly ? check_('insecure', 'Skip server verification', 'Do not verify the TLS certificate. Use only for testing.') : null;
  const workloadIdentity = check_(
    'useAzureWorkloadIdentity',
    'Use Azure workload identity',
    'Authenticate with the workload identity of the Patrol pods instead of stored credentials (Azure Container Registry and Azure DevOps).',
  );

  const fields: Record<ConnectionMethod, ReactNode> = {
    https: (
      <div className={styles.stack}>
        {identity('Repository URL', 'https://git.example.com/org/repo.git')}
        {basicAuth(true)}
        {state.httpsAuth === 'basic' && !state.useAzureWorkloadIdentity
          ? check_('forceHTTPBasicAuth', 'Force HTTP basic authentication', 'Send credentials on the first request instead of waiting for a challenge.')
          : null}
        {workloadIdentity}
        {tls}
        {skipVerify}
        {repoOnly ? check_('enableLFS', 'Enable Git LFS', 'Fetch large files stored with Git LFS.') : null}
        {network(true)}
      </div>
    ),
    ssh: (
      <div className={styles.stack}>
        {identity('Repository URL', 'git@git.example.com:org/repo.git')}
        {area('sshPrivateKey', 'SSH private key', {
          required: !editing,
          placeholder: '-----BEGIN OPENSSH PRIVATE KEY-----',
          help: secretHelp ?? 'The matching public key must have read access (and write access for write repositories).',
        })}
        {repoOnly ? check_('insecureIgnoreHostKey', 'Skip host key verification', 'Accept any SSH host key.') : null}
        {state.insecureIgnoreHostKey ? (
          <Banner tone="warning" title="Host key verification is off">
            Anyone who can intercept the connection could impersonate the server and read or alter manifests. Add the server&apos;s key
            under <Link to="/settings/certs">Certificates and known hosts</Link> instead.
          </Banner>
        ) : null}
        {repoOnly ? check_('enableLFS', 'Enable Git LFS') : null}
        {network(true)}
      </div>
    ),
    githubApp: (
      <div className={styles.stack}>
        {identity('Repository URL', 'https://github.com/org/repo.git')}
        <RadioGroup
          legend="GitHub App credentials"
          value={state.githubSource}
          onChange={(v) => set('githubSource', v)}
          options={[
            { value: 'key', label: 'Enter app credentials', description: 'App ID and private key stored in a Secret for this entry.' },
            {
              value: 'scmProvider',
              label: 'Use an SCM provider',
              description: 'Reuse the GitHub App configured on an SCM provider; rotation happens in one place.',
            },
          ]}
        />
        {state.githubSource === 'scmProvider' ? (
          <FormField
            label="SCM provider"
            id={fieldId('scmProvider')}
            required
            error={errorFor('scmProvider')}
            help={
              githubProviders.length === 0 ? (
                <>
                  No GitHub providers exist yet. Create one under <Link to="/settings/scmproviders">SCM providers</Link>.
                </>
              ) : (
                'Only GitHub providers are listed.'
              )
            }
          >
            <Select
              value={state.scmProvider}
              onChange={(e) => set('scmProvider', e.target.value)}
              placeholder="Choose a provider"
              options={githubProviders.map((p) => ({ value: p.metadata.name ?? '', label: p.metadata.name ?? '' }))}
            />
          </FormField>
        ) : (
          <>
            <div className={styles.formGrid}>
              {text('appID', 'App ID', { required: true, placeholder: '123456' })}
              {text('installationID', 'Installation ID', {
                placeholder: 'Discovered automatically',
                help: 'Optional. When empty, Patrol looks up the installation of the app on the repository owner, which needs the app to be installed there.',
              })}
            </div>
            {text('enterpriseBaseURL', 'GitHub Enterprise base URL', {
              mono: true,
              placeholder: 'https://ghe.example.com/api/v3',
              help: 'Only for GitHub Enterprise Server; leave empty for github.com.',
            })}
            {area('githubAppPrivateKey', 'Private key', { required: !editing, placeholder: '-----BEGIN RSA PRIVATE KEY-----' })}
          </>
        )}
        {tls}
        {skipVerify}
        {repoOnly ? check_('enableLFS', 'Enable Git LFS') : null}
        {network(true)}
      </div>
    ),
    helm: (
      <div className={styles.stack}>
        {identity('Chart repository URL', 'https://charts.example.com/stable', true)}
        {check_('enableOCI', 'Repository is an OCI registry', 'Charts are pulled as OCI artifacts (the URL is a registry reference).')}
        {state.enableOCI ? check_('insecureOCIForceHTTP', 'Use plain HTTP', 'Connect to the registry without TLS. Use only for local registries.') : null}
        {basicAuth(false)}
        {workloadIdentity}
        {tls}
        {skipVerify}
        {network(false)}
      </div>
    ),
    oci: (
      <div className={styles.stack}>
        {identity('OCI repository URL', 'oci://registry.example.com/org/manifests')}
        {basicAuth(false)}
        {workloadIdentity}
        {check_('insecureOCIForceHTTP', 'Use plain HTTP', 'Connect to the registry without TLS. Use only for local registries.')}
        {tls}
        {skipVerify}
        {network(false)}
      </div>
    ),
    gcp: (
      <div className={styles.stack}>
        {identity('Repository URL', 'https://source.developers.google.com/p/project/r/repo')}
        {area('gcpServiceAccountKey', 'Service account key (JSON)', {
          required: !editing,
          placeholder: '{"type": "service_account", …}',
          help: secretHelp ?? 'Key of a service account with the Source Repository Reader role.',
        })}
        {network(true)}
      </div>
    ),
    azure: (
      <div className={styles.stack}>
        {identity('Repository URL', 'https://dev.azure.com/org/project/_git/repo')}
        <div className={styles.formGrid}>
          {text('azureTenantID', 'Tenant ID', { required: true, mono: true })}
          {text('azureClientID', 'Client ID', { required: true, mono: true })}
        </div>
        {text('azureClientSecret', 'Client secret', { type: 'password', required: !editing, help: secretHelp })}
        {text('azureEndpoint', 'Active Directory endpoint', { mono: true, placeholder: 'https://login.microsoftonline.com', help: 'Only for sovereign clouds.' })}
        {skipVerify}
        {network(true)}
      </div>
    ),
  };

  const title =
    mode === 'template'
      ? purpose === 'write'
        ? 'New write credential template'
        : 'New credential template'
      : editing
        ? `Edit ${kind}`
        : purpose === 'write'
          ? 'Connect write repository'
          : 'Connect repository';
  const description =
    mode === 'template'
      ? 'Credentials applied to every repository whose URL starts with the prefix.'
      : purpose === 'write'
        ? 'Push access used by the source hydrator to commit rendered manifests. Read access is configured separately under Repositories.'
        : 'The connection is tested before it is saved.';

  const failed = result && result.state.status !== 'Successful' ? result : null;

  return (
    <Drawer
      open={open}
      onClose={onClose}
      title={title}
      description={description}
      width="720px"
      footer={
        <div className={styles.actions}>
          <Button onClick={onClose}>Cancel</Button>
          {mode === 'repository' ? (
            <Button
              onClick={() => {
                if (check()) void test(false);
              }}
              loading={validate.isPending && !result?.fromSave}
              disabled={busy}
            >
              Test connection
            </Button>
          ) : null}
          {mode === 'repository' && !editing ? (
            <Button
              onClick={() => {
                if (check()) void saveTemplate();
              }}
              disabled={busy}
            >
              Save as credential template
            </Button>
          ) : null}
          <Button variant="primary" type="submit" form={formId} loading={busy && !validate.isPending} disabled={busy}>
            {mode === 'template' ? 'Save template' : editing ? 'Save' : 'Connect'}
          </Button>
        </div>
      }
    >
      <form id={formId} className={styles.form} onSubmit={(e) => void onSubmit(e)} noValidate aria-label={title}>
        <ErrorSummary errors={form.errors} focusKey={form.attempt} />
        {saveError ? <ActionError error={saveError.error} action={saveError.action} onReload={editing ? () => void reloadLatest() : undefined} /> : null}
        {result?.state.status === 'Successful' ? (
          <Banner tone="success" live title="Connection successful">
            Patrol can reach <Mono>{state.url.trim()}</Mono> with these settings.
          </Banner>
        ) : null}
        {failed ? (
          <Banner
            tone="error"
            live
            title="Connection failed"
            actions={
              failed.fromSave ? (
                <Button size="sm" onClick={() => void save()} disabled={busy}>
                  {editing ? 'Save anyway' : 'Connect anyway'}
                </Button>
              ) : undefined
            }
          >
            <p>{failed.state.message ?? 'The server reported a failed connection without details.'}</p>
            <p>{connectionGuidance(failed.state.message)}</p>
          </Banner>
        ) : null}
        <Tabs
          label="Connection method"
          value={state.method}
          onValueChange={(v) => {
            setState((s) => ({ ...s, method: v as ConnectionMethod }));
            setResult(null);
            form.reset();
          }}
          items={methods.map((m) => ({
            id: m,
            label: `Via ${METHOD_LABELS[m]}`,
            disabled: editing && m !== state.method,
            content: m === state.method ? fields[m] : null,
          }))}
        />
        {mode === 'repository' && !editing ? (
          <p className={styles.small}>
            “Save as credential template” stores only the credentials, for every repository under{' '}
            {state.url.trim() ? <Mono>{suggestedPrefix(state.url)}</Mono> : 'the parent path of the URL'}.
          </p>
        ) : null}
      </form>
    </Drawer>
  );
}
