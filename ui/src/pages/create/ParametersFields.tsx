import { useId } from 'react';
import type { UseQueryResult } from '@tanstack/react-query';
import type { ApplicationSource, RepositoryAppDetails } from '../../api/types';
import { Button, Checkbox, FormField, IconButton, Input, Mono, Select, Spinner, TextArea } from '../../design';
import { IconClose, IconPlus } from '../../design/icons';
import { explicitTool, produce, type ToolType } from './model';
import styles from './Create.module.css';

const TOOL_TYPES: ToolType[] = ['Directory', 'Helm', 'Kustomize', 'Plugin'];

function detectedType(details: RepositoryAppDetails | undefined): ToolType | null {
  const t = details?.type;
  return t === 'Helm' || t === 'Kustomize' || t === 'Plugin' || t === 'Directory' ? t : null;
}

function lines(value: string): string[] {
  return value
    .split('\n')
    .map((l) => l.trim())
    .filter((l) => l !== '');
}

/**
 * Tool parameters of one source: the detected type (GetAppDetails) prefills Helm value files
 * and parameters, Kustomize images and plugin settings; choosing a type explicitly writes its
 * section (helm, kustomize, directory or plugin) into the source.
 */
export function ParametersFields({
  idPrefix,
  source,
  onChange,
  details,
  pluginNames,
}: {
  idPrefix: string;
  source: ApplicationSource;
  onChange: (next: ApplicationSource) => void;
  details: UseQueryResult<RepositoryAppDetails>;
  pluginNames: string[];
}) {
  const pluginList = useId();
  const detected = detectedType(details.data);
  const explicit = explicitTool(source);
  const type = explicit ?? detected;
  const update = (recipe: (s: ApplicationSource) => void) => onChange(produce(source, recipe));

  const setType = (value: string) =>
    update((s) => {
      delete s.helm;
      delete s.kustomize;
      delete s.directory;
      delete s.plugin;
      if (value === 'Helm') s.helm = {};
      else if (value === 'Kustomize') s.kustomize = {};
      else if (value === 'Directory') s.directory = {};
      else if (value === 'Plugin') s.plugin = {};
    });

  const helmDetected = details.data?.helm;
  const helmParams = source.helm?.parameters ?? [];
  const detectedParams = (helmDetected?.parameters ?? []).filter((p): p is { name: string; value?: string } => Boolean(p.name));
  const customParams = helmParams
    .map((param, index) => ({ param, index }))
    .filter(({ param }) => !detectedParams.some((d) => d.name === param.name));
  const setParamAt = (index: number, patch: { name?: string; value?: string }) =>
    update((s) => {
      const target = s.helm?.parameters?.[index];
      if (target) Object.assign(target, patch);
    });
  const setHelmParam = (name: string, value: string) =>
    update((s) => {
      s.helm ??= {};
      const rest = (s.helm.parameters ?? []).filter((p) => p.name !== name);
      s.helm.parameters = value === '' ? rest : [...rest, { name, value }];
    });
  const valueFiles = [...new Set([...(helmDetected?.valueFiles ?? []), ...(source.helm?.valueFiles ?? [])])];

  return (
    <fieldset className={styles.subFieldset}>
      <legend className={styles.subLegend}>Parameters</legend>
      <div className={styles.row}>
        <FormField
          label="Source type"
          id={`${idPrefix}.type`}
          help={
            details.isFetching ? (
              <span className={styles.inline}>
                <Spinner size="sm" label="" /> Detecting the source type…
              </span>
            ) : detected ? (
              `Detected: ${detected}.`
            ) : details.isError ? (
              'The source type could not be detected; choose it, or leave it to the server.'
            ) : (
              'Detected once the repository and path are set.'
            )
          }
        >
          <Select value={explicit ?? ''} onChange={(e) => setType(e.target.value)}>
            <option value="">{detected ? `Auto (${detected})` : 'Auto-detect'}</option>
            {TOOL_TYPES.map((t) => (
              <option key={t} value={t}>
                {t}
              </option>
            ))}
          </Select>
        </FormField>
      </div>

      {type === 'Helm' ? (
        <div className={styles.stack}>
          <FormField label="Release name" id={`${idPrefix}.helm.releaseName`} help="Defaults to the application name.">
            <Input mono value={source.helm?.releaseName ?? ''} onChange={(e) => update((s) => void ((s.helm ??= {}).releaseName = e.target.value))} />
          </FormField>
          {valueFiles.length > 0 ? (
            <fieldset className={styles.checkGroup}>
              <legend className={styles.label}>Value files</legend>
              {valueFiles.map((file) => (
                <Checkbox
                  key={file}
                  label={<Mono>{file}</Mono>}
                  checked={(source.helm?.valueFiles ?? []).includes(file)}
                  onChange={(e) =>
                    update((s) => {
                      s.helm ??= {};
                      const current = s.helm.valueFiles ?? [];
                      s.helm.valueFiles = e.target.checked ? [...current, file] : current.filter((f) => f !== file);
                    })
                  }
                />
              ))}
            </fieldset>
          ) : null}
          <FormField label="Values" id={`${idPrefix}.helm.values`} help={helmDetected?.values ? 'Chart defaults are shown as a placeholder; enter only overrides.' : undefined}>
            <TextArea
              mono
              rows={6}
              spellCheck={false}
              placeholder={helmDetected?.values ?? 'replicaCount: 2'}
              value={source.helm?.values ?? ''}
              onChange={(e) => update((s) => void ((s.helm ??= {}).values = e.target.value))}
            />
          </FormField>
          {detectedParams.length > 0 || customParams.length > 0 ? (
            <div className={styles.paramTable} role="group" aria-label="Helm parameters">
              {detectedParams.map((p) => (
                <FormField key={p.name} label={<Mono>{p.name}</Mono>} id={`${idPrefix}.helm.param.${p.name}`}>
                  <Input
                    mono
                    placeholder={p.value ?? ''}
                    value={helmParams.find((h) => h.name === p.name)?.value ?? ''}
                    onChange={(e) => setHelmParam(p.name, e.target.value)}
                  />
                </FormField>
              ))}
              {customParams.map(({ param, index }) => (
                <div key={index} className={styles.inlineRow}>
                  <FormField label="Parameter" id={`${idPrefix}.helm.custom.${index}.name`}>
                    <Input mono value={param.name ?? ''} onChange={(e) => setParamAt(index, { name: e.target.value })} />
                  </FormField>
                  <FormField label="Value" id={`${idPrefix}.helm.custom.${index}.value`}>
                    <Input mono value={param.value ?? ''} onChange={(e) => setParamAt(index, { value: e.target.value })} />
                  </FormField>
                  <IconButton
                    aria-label={`Remove parameter ${param.name === undefined || param.name === '' ? String(index + 1) : param.name}`}
                    icon={<IconClose />}
                    className={styles.alignEnd}
                    onClick={() => update((s) => void (s.helm && (s.helm.parameters = (s.helm.parameters ?? []).filter((_, i) => i !== index))))}
                  />
                </div>
              ))}
            </div>
          ) : null}
          <div>
            <Button
              size="sm"
              icon={<IconPlus />}
              onClick={() =>
                update((s) => {
                  s.helm ??= {};
                  s.helm.parameters = [...(s.helm.parameters ?? []), { name: '', value: '' }];
                })
              }
            >
              Add parameter
            </Button>
          </div>
        </div>
      ) : null}

      {type === 'Kustomize' ? (
        <div className={styles.stack}>
          <div className={styles.row}>
            <FormField label="Name prefix" id={`${idPrefix}.kustomize.namePrefix`}>
              <Input mono value={source.kustomize?.namePrefix ?? ''} onChange={(e) => update((s) => void ((s.kustomize ??= {}).namePrefix = e.target.value))} />
            </FormField>
            <FormField label="Name suffix" id={`${idPrefix}.kustomize.nameSuffix`}>
              <Input mono value={source.kustomize?.nameSuffix ?? ''} onChange={(e) => update((s) => void ((s.kustomize ??= {}).nameSuffix = e.target.value))} />
            </FormField>
            <FormField label="Namespace" id={`${idPrefix}.kustomize.namespace`}>
              <Input mono value={source.kustomize?.namespace ?? ''} onChange={(e) => update((s) => void ((s.kustomize ??= {}).namespace = e.target.value))} />
            </FormField>
          </div>
          <FormField
            label="Image overrides"
            id={`${idPrefix}.kustomize.images`}
            help={
              details.data?.kustomize?.images?.length
                ? `One per line, e.g. name=new-name:tag or name:tag. Images in the kustomization: ${details.data.kustomize.images.join(', ')}.`
                : 'One per line, e.g. name=new-name:tag or name:tag.'
            }
          >
            <TextArea
              mono
              rows={3}
              spellCheck={false}
              value={(source.kustomize?.images ?? []).join('\n')}
              onChange={(e) => update((s) => void ((s.kustomize ??= {}).images = e.target.value.split('\n')))}
              onBlur={(e) => update((s) => void ((s.kustomize ??= {}).images = lines(e.target.value)))}
            />
          </FormField>
        </div>
      ) : null}

      {type === 'Directory' ? (
        <div className={styles.stack}>
          <Checkbox
            label="Recurse into subdirectories"
            checked={source.directory?.recurse ?? false}
            onChange={(e) => update((s) => void ((s.directory ??= {}).recurse = e.target.checked))}
          />
          <div className={styles.row}>
            <FormField label="Include" id={`${idPrefix}.directory.include`} help='Glob, e.g. "{config.yaml,env-*.yaml}".'>
              <Input mono value={source.directory?.include ?? ''} onChange={(e) => update((s) => void ((s.directory ??= {}).include = e.target.value))} />
            </FormField>
            <FormField label="Exclude" id={`${idPrefix}.directory.exclude`}>
              <Input mono value={source.directory?.exclude ?? ''} onChange={(e) => update((s) => void ((s.directory ??= {}).exclude = e.target.value))} />
            </FormField>
          </div>
        </div>
      ) : null}

      {type === 'Plugin' ? (
        <div className={styles.stack}>
          <datalist id={pluginList}>
            {pluginNames.map((n) => (
              <option key={n} value={n} />
            ))}
          </datalist>
          <FormField label="Plugin name" id={`${idPrefix}.plugin.name`} help="Leave empty to let the repo server discover the plugin.">
            <Input mono list={pluginList} autoComplete="off" value={source.plugin?.name ?? ''} onChange={(e) => update((s) => void ((s.plugin ??= {}).name = e.target.value))} />
          </FormField>
          <FormField label="Environment" id={`${idPrefix}.plugin.env`} help="One NAME=value per line.">
            <TextArea
              mono
              rows={3}
              spellCheck={false}
              defaultValue={(source.plugin?.env ?? []).map((e) => `${e.name}=${e.value}`).join('\n')}
              onBlur={(e) =>
                update((s) => {
                  s.plugin ??= {};
                  s.plugin.env = lines(e.target.value).map((l) => {
                    const at = l.indexOf('=');
                    return at === -1 ? { name: l, value: '' } : { name: l.slice(0, at), value: l.slice(at + 1) };
                  });
                })
              }
            />
          </FormField>
        </div>
      ) : null}
    </fieldset>
  );
}
