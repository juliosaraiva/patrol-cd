import { useEffect, useId, useMemo, useRef, useState, type RefObject } from 'react';
import { useNavigate } from 'react-router';
import { isApiError } from '../../api/errors';
import { useCreateApplication } from '../../api/queries/applications';
import { useClusters, usePluginSettings, useProjects, useRepositories, useSettings } from '../../api/queries/resources';
import type { Application, ApplicationSource } from '../../api/types';
import { Banner, Button, Checkbox, FormField, Input, RadioGroup, Select, Switch, TextArea, useToast } from '../../design';
import { IconPlus } from '../../design/icons';
import { ErrorBanner } from '../../shell/errors';
import {
  cleanApplication,
  createdApplicationPath,
  defaultApplication,
  defaultSource,
  isMultiSource,
  parseApplicationYAML,
  produce,
  PRUNE_PROPAGATION,
  RESOURCES_FINALIZER,
  setMultiSource,
  setSyncOption,
  sourcesOf,
  SYNC_OPTIONS,
  syncOptionValue,
  toYAML,
  validateApplication,
  yamlFieldPath,
  type HydratorSpec,
} from './model';
import { SourceFields } from './SourceFields';
import styles from './Create.module.css';

export interface ApplicationCreateFormProps {
  /** Form element id, so a submit button outside the form (drawer footer) can use form={id}. */
  formId: string;
  initial?: { repoURL?: string; project?: string } | undefined;
  onCancel: () => void;
  /** Called after the server created the application (before navigating to it). */
  onCreated?: ((app: Application) => void) | undefined;
  /** Receives the application name input, so a containing dialog can focus it first. */
  nameInputRef?: RefObject<HTMLInputElement | null> | undefined;
}

type Mode = 'form' | 'yaml';

/** Messages for server errors on create, with the next step. */
function describeCreateError(error: unknown, app: Application): { title: string; message: string } | null {
  if (!isApiError(error)) return null;
  switch (error.code) {
    case 'already_exists':
    case 'conflict':
      return {
        title: `An application named ${app.metadata.name ?? ''} already exists`,
        message: `${error.detail}. Choose another name, or turn on "Upsert" to replace the existing application with this spec.`,
      };
    case 'invalid_argument':
      return { title: 'The server rejected the application', message: `${error.detail}. Correct the spec and create it again.` };
    case 'permission_denied':
      return {
        title: 'You cannot create applications in this project',
        message: `${error.detail}. Ask an administrator for applications, create on project ${app.spec.project}, or choose another project.`,
      };
    case 'failed_precondition':
      return { title: 'The project does not allow this application', message: `${error.detail}. Check the project's source repositories and destinations.` };
    default:
      return null;
  }
}

/**
 * Application creation (API-091): form and YAML modes over one Application draft, upsert and
 * validate options, then POST /applications and navigation to the new application.
 */
export function ApplicationCreateForm({ formId, initial, onCancel, onCreated, nameInputRef }: ApplicationCreateFormProps) {
  const navigate = useNavigate();
  const { toast } = useToast();
  const ids = { clusters: useId(), clusterNames: useId(), errors: useId() };
  const settings = useSettings();
  const projects = useProjects();
  const repos = useRepositories();
  const clusters = useClusters();
  const plugins = usePluginSettings();
  const create = useCreateApplication();

  const [app, setApp] = useState<Application>(() => defaultApplication(initial));
  const [mode, setMode] = useState<Mode>('form');
  const [yamlText, setYamlText] = useState('');
  const [yamlError, setYamlError] = useState<string | null>(null);
  const [yamlEdited, setYamlEdited] = useState(false);
  const [destinationBy, setDestinationBy] = useState<'server' | 'name'>('server');
  const [upsert, setUpsert] = useState(false);
  const [validate, setValidate] = useState(true);
  const [submitted, setSubmitted] = useState(false);
  const [failedAttempt, setFailedAttempt] = useState(0);
  const formRef = useRef<HTMLFormElement>(null);
  const summaryRef = useRef<HTMLDivElement>(null);

  const cp = settings.data?.controllerNamespace;
  const repoList = useMemo(() => repos.data?.items ?? [], [repos.data]);
  const pluginNames = plugins.data?.plugins?.map((p) => p.name) ?? [];
  const appNamespaceAllowed = settings.data?.appsInAnyNamespaceEnabled === true;
  const errors = validateApplication(app, { repos: repoList, appNamespaceAllowed });
  // In YAML mode messages name the document path, since the form fields are not shown.
  const errorList = Object.entries(errors).map(([key, message]): [string, string] => [key, mode === 'yaml' ? `${yamlFieldPath(key, app.spec)}: ${message}` : message]);
  const showErrors = submitted;
  const err = (key: string) => (showErrors ? errors[key] : undefined);

  const update = (recipe: (draft: Application) => void) => setApp((current) => produce(current, recipe));
  const spec = app.spec;
  const multi = isMultiSource(spec);
  const sources = sourcesOf(spec);
  const hydrator = spec.sourceHydrator;
  const automated = spec.syncPolicy?.automated;
  const autoSync = automated !== undefined && automated.enabled !== false;
  const syncOptions = spec.syncPolicy?.syncOptions;

  const setSource = (index: number, next: ApplicationSource) =>
    update((d) => {
      if (isMultiSource(d.spec)) (d.spec.sources ?? [])[index] = next;
      else d.spec.source = next;
    });

  const switchMode = (next: Mode) => {
    if (next === mode) return;
    if (next === 'yaml') {
      setYamlText(toYAML(app));
      setYamlError(null);
      setYamlEdited(false);
      setMode('yaml');
      return;
    }
    const parsed = parseApplicationYAML(yamlText);
    if ('error' in parsed) {
      setYamlError(parsed.error);
      return;
    }
    // The single-source form needs a source to edit; YAML may have none yet.
    const draft = parsed.app.spec.sourceHydrator || isMultiSource(parsed.app.spec) || sourcesOf(parsed.app.spec).length > 0 ? parsed.app : produce(parsed.app, (d) => void (d.spec.source = defaultSource()));
    setApp(draft);
    if (draft.spec.destination.name && !draft.spec.destination.server) setDestinationBy('name');
    setMode('form');
  };

  const onYamlChange = (text: string) => {
    setYamlText(text);
    setYamlEdited(true);
    const parsed = parseApplicationYAML(text);
    if ('error' in parsed) setYamlError(parsed.error);
    else {
      setYamlError(null);
      setApp(parsed.app);
    }
  };

  const submit = () => {
    setSubmitted(true);
    if ((mode === 'yaml' && yamlError) || errorList.length > 0) {
      // Focus moves to the first invalid field (or the summary) once the errors are rendered.
      setFailedAttempt((n) => n + 1);
      return;
    }
    create.mutate(
      { app: cleanApplication(app), upsert, validate },
      {
        onSuccess: (created) => {
          const name = created.metadata.name ?? app.metadata.name ?? '';
          toast({ tone: 'success', title: `Application ${name} created` });
          onCreated?.(created);
          void navigate(createdApplicationPath({ ...created, metadata: { ...created.metadata, name } }, cp));
        },
      },
    );
  };

  // After a failed submit, focus the first invalid field (form mode) or the error summary.
  useEffect(() => {
    if (failedAttempt === 0) return;
    const first = mode === 'form' ? formRef.current?.querySelector<HTMLElement>('[aria-invalid="true"]') : null;
    (first ?? summaryRef.current)?.focus();
    // Only after a new failed attempt.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [failedAttempt]);

  // The YAML editor shows the syntax/shape error, else (once edited or submitted) the first
  // problem that blocks creation, so a half-finished edit is explained where it is made.
  const firstProblem = errorList[0]?.[1];
  const yamlFieldError =
    yamlError ??
    ((yamlEdited || submitted) && firstProblem
      ? `${firstProblem}${errorList.length > 1 ? ` (${errorList.length - 1} more problem${errorList.length > 2 ? 's' : ''})` : ''}`
      : undefined);

  const friendly = create.isError ? describeCreateError(create.error, app) : null;
  const projectOptions = (projects.data?.items ?? []).map((p) => p.metadata.name ?? '').filter(Boolean);
  if (spec.project && !projectOptions.includes(spec.project)) projectOptions.unshift(spec.project);

  const toggleHydrator = (on: boolean) =>
    update((d) => {
      if (on) {
        const first = sourcesOf(d.spec)[0] ?? defaultSource();
        const h: HydratorSpec = {
          drySource: { repoURL: first.repoURL, targetRevision: first.targetRevision ?? 'HEAD', path: first.path ?? '.' },
          syncSource: { targetBranch: 'environments/dev', path: first.path ?? '' },
        };
        d.spec.sourceHydrator = h;
        delete d.spec.source;
        delete d.spec.sources;
      } else {
        const dry = d.spec.sourceHydrator?.drySource;
        d.spec.source = { repoURL: dry?.repoURL ?? '', targetRevision: dry?.targetRevision ?? 'HEAD', path: dry?.path ?? '' };
        delete d.spec.sourceHydrator;
      }
    });

  return (
    <form
      id={formId}
      ref={formRef}
      className={styles.form}
      noValidate
      onSubmit={(e) => {
        e.preventDefault();
        submit();
      }}
    >
      <div className={styles.modeBar} role="group" aria-label="Editor mode">
        <Button size="sm" variant={mode === 'form' ? 'primary' : 'secondary'} aria-pressed={mode === 'form'} onClick={() => switchMode('form')}>
          Form
        </Button>
        <Button size="sm" variant={mode === 'yaml' ? 'primary' : 'secondary'} aria-pressed={mode === 'yaml'} onClick={() => switchMode('yaml')}>
          Edit as YAML
        </Button>
      </div>

      {mode === 'yaml' ? (
        <FormField
          label="Application manifest (YAML)"
          error={yamlFieldError}
          help="Edits here and in the form apply to the same application; switch back to the form at any time."
        >
          <TextArea mono rows={28} spellCheck={false} className={styles.yaml} value={yamlText} onChange={(e) => onYamlChange(e.target.value)} />
        </FormField>
      ) : (
        <>
          <fieldset className={styles.fieldset}>
            <legend className={styles.legend}>General</legend>
            <div className={styles.row}>
              <FormField label="Application name" required id="name" error={err('name')} className={styles.grow}>
                <Input ref={nameInputRef} mono autoComplete="off" value={app.metadata.name ?? ''} onChange={(e) => update((d) => void (d.metadata.name = e.target.value))} />
              </FormField>
              <FormField label="Project" required id="project" error={err('project')} help={projects.isError ? 'Projects could not be loaded; type the project name in YAML mode.' : undefined}>
                <Select value={spec.project} onChange={(e) => update((d) => void (d.spec.project = e.target.value))}>
                  {projectOptions.length === 0 ? <option value="">No projects</option> : null}
                  {projectOptions.map((p) => (
                    <option key={p} value={p}>
                      {p}
                    </option>
                  ))}
                </Select>
              </FormField>
            </div>
            {appNamespaceAllowed ? (
              <FormField
                label="Application namespace"
                id="appNamespace"
                error={err('appNamespace')}
                help={`Namespace of the Application object. Empty means ${cp ?? 'the control-plane namespace'}; other namespaces must be allowed by the project's source namespaces.`}
              >
                <Input mono placeholder={cp} value={app.metadata.namespace ?? ''} onChange={(e) => update((d) => void (d.metadata.namespace = e.target.value || undefined))} />
              </FormField>
            ) : null}
            <Checkbox
              label="Delete resources when the application is deleted"
              description={`Adds the ${RESOURCES_FINALIZER} finalizer so deletion cascades to the deployed resources.`}
              checked={(app.metadata.finalizers ?? []).includes(RESOURCES_FINALIZER)}
              onChange={(e) =>
                update((d) => {
                  const rest = (d.metadata.finalizers ?? []).filter((f) => f !== RESOURCES_FINALIZER);
                  d.metadata.finalizers = e.target.checked ? [...rest, RESOURCES_FINALIZER] : rest;
                })
              }
            />
          </fieldset>

          <fieldset className={styles.fieldset}>
            <legend className={styles.legend}>Sync policy</legend>
            <RadioGroup
              legend="Sync"
              orientation="horizontal"
              value={autoSync ? 'automatic' : 'manual'}
              onChange={(v) =>
                update((d) => {
                  d.spec.syncPolicy ??= {};
                  if (v === 'automatic') d.spec.syncPolicy.automated = { prune: false, selfHeal: false };
                  else delete d.spec.syncPolicy.automated;
                })
              }
              options={[
                { value: 'manual', label: 'Manual', description: 'Sync when someone requests it.' },
                { value: 'automatic', label: 'Automatic', description: 'Sync whenever the desired state changes.' },
              ]}
            />
            {autoSync ? (
              <div className={styles.checks}>
                <Checkbox
                  label="Prune resources"
                  description="Delete resources that are no longer in the source."
                  checked={automated.prune ?? false}
                  onChange={(e) => update((d) => void ((d.spec.syncPolicy ??= {}).automated = { ...d.spec.syncPolicy?.automated, prune: e.target.checked }))}
                />
                <Checkbox
                  label="Self heal"
                  description="Revert changes made to live resources outside Patrol."
                  checked={automated.selfHeal ?? false}
                  onChange={(e) => update((d) => void ((d.spec.syncPolicy ??= {}).automated = { ...d.spec.syncPolicy?.automated, selfHeal: e.target.checked }))}
                />
                <Checkbox
                  label="Allow empty"
                  description="Allow syncs that would delete every resource."
                  checked={automated.allowEmpty ?? false}
                  onChange={(e) => update((d) => void ((d.spec.syncPolicy ??= {}).automated = { ...d.spec.syncPolicy?.automated, allowEmpty: e.target.checked }))}
                />
              </div>
            ) : null}
            <fieldset className={styles.subFieldset}>
              <legend className={styles.subLegend}>Sync options</legend>
              <div className={styles.checks}>
                {SYNC_OPTIONS.filter((o) => !o.replace || settings.data?.syncWithReplaceAllowed).map((o) => (
                  <Checkbox
                    key={o.key}
                    label={o.label}
                    description={o.description}
                    checked={syncOptionValue(syncOptions, o.key) === o.on}
                    onChange={(e) =>
                      update((d) => {
                        d.spec.syncPolicy ??= {};
                        d.spec.syncPolicy.syncOptions = setSyncOption(d.spec.syncPolicy.syncOptions, o.key, e.target.checked ? o.on : undefined);
                      })
                    }
                  />
                ))}
              </div>
              <FormField label="Prune propagation policy" id="prunePropagation">
                <Select
                  value={syncOptionValue(syncOptions, 'PrunePropagationPolicy') ?? ''}
                  onChange={(e) =>
                    update((d) => {
                      d.spec.syncPolicy ??= {};
                      d.spec.syncPolicy.syncOptions = setSyncOption(d.spec.syncPolicy.syncOptions, 'PrunePropagationPolicy', e.target.value || undefined);
                    })
                  }
                >
                  <option value="">Default (foreground)</option>
                  {PRUNE_PROPAGATION.map((p) => (
                    <option key={p} value={p}>
                      {p}
                    </option>
                  ))}
                </Select>
              </FormField>
            </fieldset>
            <Checkbox
              label="Retry failed syncs"
              checked={spec.syncPolicy?.retry !== undefined}
              onChange={(e) =>
                update((d) => {
                  d.spec.syncPolicy ??= {};
                  if (e.target.checked) d.spec.syncPolicy.retry = { limit: 5, backoff: { duration: '5s', factor: 2, maxDuration: '3m' } };
                  else delete d.spec.syncPolicy.retry;
                })
              }
            />
            {spec.syncPolicy?.retry ? (
              <div className={styles.row}>
                <FormField label="Limit" id="retry.limit" help="Attempts; -1 retries forever.">
                  <Input type="number" value={String(spec.syncPolicy.retry.limit ?? '')} onChange={(e) => update((d) => void ((d.spec.syncPolicy ??= {}).retry = { ...d.spec.syncPolicy?.retry, limit: Number(e.target.value) }))} />
                </FormField>
                <FormField label="Backoff" id="retry.duration" help='Initial delay, e.g. "5s".'>
                  <Input mono value={spec.syncPolicy.retry.backoff?.duration ?? ''} onChange={(e) => update((d) => void ((((d.spec.syncPolicy ??= {}).retry ??= {}).backoff ??= {}).duration = e.target.value))} />
                </FormField>
                <FormField label="Factor" id="retry.factor">
                  <Input type="number" value={String(spec.syncPolicy.retry.backoff?.factor ?? '')} onChange={(e) => update((d) => void ((((d.spec.syncPolicy ??= {}).retry ??= {}).backoff ??= {}).factor = Number(e.target.value)))} />
                </FormField>
                <FormField label="Max backoff" id="retry.maxDuration">
                  <Input mono value={spec.syncPolicy.retry.backoff?.maxDuration ?? ''} onChange={(e) => update((d) => void ((((d.spec.syncPolicy ??= {}).retry ??= {}).backoff ??= {}).maxDuration = e.target.value))} />
                </FormField>
              </div>
            ) : null}
          </fieldset>

          <fieldset className={styles.fieldset}>
            <legend className={styles.legend}>{hydrator ? 'Source hydrator' : multi ? 'Sources' : 'Source'}</legend>
            <div className={styles.switches}>
              {!hydrator ? <Switch label="Multiple sources" checked={multi} onChange={(on) => update((d) => void (d.spec = setMultiSource(d.spec, on)))} /> : null}
              {settings.data?.hydratorEnabled ? <Switch label="Use the source hydrator" checked={hydrator !== undefined} onChange={toggleHydrator} /> : null}
            </div>
            {showErrors && errors.sources ? <p className={styles.error}>{errors.sources}</p> : null}
            {hydrator ? (
              <div className={styles.stack}>
                <p className={styles.muted}>
                  The hydrator renders the dry source and commits the plain manifests to the sync branch; the application deploys from there.
                </p>
                <div className={styles.row}>
                  <FormField label="Dry source repository URL" required id="hydrator.repoURL" error={err('hydrator.repoURL')} className={styles.grow}>
                    <Input mono value={hydrator.drySource.repoURL} onChange={(e) => update((d) => void (d.spec.sourceHydrator && (d.spec.sourceHydrator.drySource.repoURL = e.target.value)))} />
                  </FormField>
                  <FormField label="Dry source revision" id="hydrator.revision">
                    <Input mono value={hydrator.drySource.targetRevision} onChange={(e) => update((d) => void (d.spec.sourceHydrator && (d.spec.sourceHydrator.drySource.targetRevision = e.target.value)))} />
                  </FormField>
                  <FormField label="Dry source path" id="hydrator.path">
                    <Input mono value={hydrator.drySource.path} onChange={(e) => update((d) => void (d.spec.sourceHydrator && (d.spec.sourceHydrator.drySource.path = e.target.value)))} />
                  </FormField>
                </div>
                <div className={styles.row}>
                  <FormField label="Sync branch" required id="hydrator.syncBranch" error={err('hydrator.syncBranch')}>
                    <Input mono value={hydrator.syncSource.targetBranch} onChange={(e) => update((d) => void (d.spec.sourceHydrator && (d.spec.sourceHydrator.syncSource.targetBranch = e.target.value)))} />
                  </FormField>
                  <FormField label="Sync path" required id="hydrator.syncPath" error={err('hydrator.syncPath')} className={styles.grow}>
                    <Input mono value={hydrator.syncSource.path} onChange={(e) => update((d) => void (d.spec.sourceHydrator && (d.spec.sourceHydrator.syncSource.path = e.target.value)))} />
                  </FormField>
                  <FormField label="Hydrate to branch" id="hydrator.hydrateTo" help="Optional staging branch; promote by merging it into the sync branch.">
                    <Input
                      mono
                      value={hydrator.hydrateTo?.targetBranch ?? ''}
                      onChange={(e) =>
                        update((d) => {
                          if (!d.spec.sourceHydrator) return;
                          if (e.target.value) d.spec.sourceHydrator.hydrateTo = { targetBranch: e.target.value };
                          else delete d.spec.sourceHydrator.hydrateTo;
                        })
                      }
                    />
                  </FormField>
                </div>
              </div>
            ) : (
              <>
                {sources.map((src, i) => (
                  <SourceFields
                    key={i}
                    index={i}
                    multi={multi}
                    source={src}
                    onChange={(next) => setSource(i, next)}
                    onRemove={multi && sources.length > 1 ? () => update((d) => void (d.spec.sources = (d.spec.sources ?? []).filter((_, j) => j !== i))) : undefined}
                    repos={repoList}
                    project={spec.project}
                    appName={app.metadata.name ?? ''}
                    pluginNames={pluginNames}
                    errors={errors}
                    showErrors={showErrors}
                  />
                ))}
                {multi ? (
                  <div>
                    <Button size="sm" icon={<IconPlus />} onClick={() => update((d) => void (d.spec.sources = [...(d.spec.sources ?? []), defaultSource()]))}>
                      Add source
                    </Button>
                  </div>
                ) : null}
              </>
            )}
          </fieldset>

          <fieldset className={styles.fieldset}>
            <legend className={styles.legend}>Destination</legend>
            <RadioGroup
              legend="Choose the cluster by"
              orientation="horizontal"
              value={destinationBy}
              onChange={(v) => {
                setDestinationBy(v);
                update((d) => {
                  if (v === 'server') delete d.spec.destination.name;
                  else delete d.spec.destination.server;
                });
              }}
              options={[
                { value: 'server', label: 'URL' },
                { value: 'name', label: 'Name' },
              ]}
            />
            <datalist id={ids.clusters}>
              {(clusters.data?.items ?? []).map((c) => (
                <option key={c.spec.server} value={c.spec.server} label={c.spec.name} />
              ))}
            </datalist>
            <datalist id={ids.clusterNames}>
              {(clusters.data?.items ?? [])
                .filter((c) => c.spec.name)
                .map((c) => (
                  <option key={c.spec.name} value={c.spec.name} label={c.spec.server} />
                ))}
            </datalist>
            <div className={styles.row}>
              {destinationBy === 'server' ? (
                <FormField label="Cluster URL" required id="destination.target" error={err('destination.target')} className={styles.grow} help="https://kubernetes.default.svc is the cluster Patrol runs in.">
                  <Input mono list={ids.clusters} autoComplete="off" value={spec.destination.server ?? ''} onChange={(e) => update((d) => void (d.spec.destination.server = e.target.value))} />
                </FormField>
              ) : (
                <FormField label="Cluster name" required id="destination.target" error={err('destination.target')} className={styles.grow}>
                  <Input mono list={ids.clusterNames} autoComplete="off" value={spec.destination.name ?? ''} onChange={(e) => update((d) => void (d.spec.destination.name = e.target.value))} />
                </FormField>
              )}
              <FormField label="Namespace" id="destination.namespace" error={err('destination.namespace')} help="For namespaced resources that set none.">
                <Input mono value={spec.destination.namespace ?? ''} onChange={(e) => update((d) => void (d.spec.destination.namespace = e.target.value))} />
              </FormField>
            </div>
          </fieldset>
        </>
      )}

      <fieldset className={styles.fieldset}>
        <legend className={styles.legend}>Create options</legend>
        <div className={styles.checks}>
          <Checkbox label="Upsert" description="Replace an existing application with the same name instead of failing." checked={upsert} onChange={(e) => setUpsert(e.target.checked)} />
          <Checkbox
            label="Validate"
            description="Check on the server that the repository, path and destination are usable before creating."
            checked={validate}
            onChange={(e) => setValidate(e.target.checked)}
          />
        </div>
      </fieldset>

      <div ref={summaryRef} tabIndex={-1} className={styles.summary}>
        {submitted && (errorList.length > 0 || (mode === 'yaml' && yamlError)) ? (
          <Banner tone="error" live title="The application cannot be created yet">
            <ul id={ids.errors} className={styles.errorList}>
              {mode === 'yaml' && yamlError ? <li>{yamlError}</li> : null}
              {errorList.map(([key, message]) => (
                <li key={key}>{message}</li>
              ))}
            </ul>
          </Banner>
        ) : null}
        {create.isError ? (
          friendly ? (
            <Banner tone="error" live title={friendly.title}>
              {friendly.message}
            </Banner>
          ) : (
            <ErrorBanner error={create.error} title="The application was not created" />
          )
        ) : null}
      </div>

      <div className={styles.inlineActions}>
        <Button type="submit" variant="primary" loading={create.isPending}>
          Create
        </Button>
        <Button onClick={onCancel}>Cancel</Button>
      </div>
    </form>
  );
}
