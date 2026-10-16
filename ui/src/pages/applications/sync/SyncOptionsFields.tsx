import { useId } from 'react';
import { Checkbox, FormField, Input, Select } from '../../../design';
import styles from './Sync.module.css';
import {
  isBooleanOptionChecked,
  optionValue,
  setBooleanOption,
  setSelectOption,
  syncOptionCatalog,
  type RetryForm,
  type validateRetry,
} from './syncOptions';

/** Every sync option with its explanation; Replace only when the server allows it. */
export function SyncOptionsFields({
  options,
  onChange,
  replaceAllowed,
  legend = 'Sync options',
}: {
  options: readonly string[];
  onChange: (next: string[]) => void;
  replaceAllowed: boolean;
  legend?: string;
}) {
  const catalog = syncOptionCatalog(replaceAllowed);
  return (
    <fieldset className={styles.fieldset}>
      <legend>{legend}</legend>
      <div className={styles.stack}>
        {catalog.map((def) =>
          def.kind === 'boolean' ? (
            <Checkbox
              key={def.key}
              label={def.label}
              description={def.description}
              checked={isBooleanOptionChecked(options, def)}
              onChange={(e) => onChange(setBooleanOption(options, def, e.target.checked))}
            />
          ) : (
            <FormField key={def.key} label={def.label} help={def.description}>
              <Select
                value={optionValue(options, def.key) ?? def.defaultValue}
                onChange={(e) => onChange(setSelectOption(options, def, e.target.value))}
                options={def.values.map((v) => ({ value: v, label: v }))}
              />
            </FormField>
          ),
        )}
      </div>
    </fieldset>
  );
}

export interface ManualFlags {
  prune: boolean;
  dryRun: boolean;
  applyOnly: boolean;
  force: boolean;
}

const FLAG_HELP: Record<keyof ManualFlags, { label: string; description: string }> = {
  prune: { label: 'Prune', description: 'Delete live resources that are no longer defined in the source.' },
  dryRun: { label: 'Dry run', description: 'Validate the sync with server-side dry runs; nothing changes in the cluster.' },
  applyOnly: { label: 'Apply only', description: 'Apply manifests without running sync hooks (PreSync, Sync, PostSync).' },
  force: {
    label: 'Force',
    description: 'Delete and recreate resources that cannot be patched. Destructive: bypasses graceful deletion and can cause downtime.',
  },
};

export function ManualFlagsFields({ value, onChange }: { value: ManualFlags; onChange: (next: ManualFlags) => void }) {
  return (
    <fieldset className={styles.fieldset}>
      <legend>Sync flags</legend>
      <div className={styles.stack}>
        {(Object.keys(FLAG_HELP) as (keyof ManualFlags)[]).map((key) => (
          <Checkbox
            key={key}
            label={FLAG_HELP[key].label}
            description={FLAG_HELP[key].description}
            checked={value[key]}
            onChange={(e) => onChange({ ...value, [key]: e.target.checked })}
          />
        ))}
      </div>
    </fieldset>
  );
}

export function RetryFields({
  value,
  onChange,
  errors,
  legend = 'Retry',
}: {
  value: RetryForm;
  onChange: (next: RetryForm) => void;
  errors: ReturnType<typeof validateRetry>;
  legend?: string;
}) {
  const id = useId();
  return (
    <fieldset className={styles.fieldset}>
      <legend>{legend}</legend>
      <div className={styles.stack}>
        <Checkbox
          label="Retry failed syncs"
          description="Retry with exponential backoff when the sync fails."
          checked={value.enabled}
          onChange={(e) => onChange({ ...value, enabled: e.target.checked })}
        />
        {value.enabled ? (
          <div className={styles.grid2}>
            <FormField label="Limit" error={errors.limit} help="Number of retries; a negative number retries forever." id={`${id}-limit`}>
              <Input inputMode="numeric" value={value.limit} onChange={(e) => onChange({ ...value, limit: e.target.value })} />
            </FormField>
            <FormField label="Backoff duration" error={errors.duration} help="Delay before the first retry, e.g. 5s." id={`${id}-duration`}>
              <Input mono value={value.duration} onChange={(e) => onChange({ ...value, duration: e.target.value })} />
            </FormField>
            <FormField label="Backoff factor" error={errors.factor} help="Multiplier applied after each retry." id={`${id}-factor`}>
              <Input inputMode="decimal" value={value.factor} onChange={(e) => onChange({ ...value, factor: e.target.value })} />
            </FormField>
            <FormField label="Maximum backoff" error={errors.maxDuration} help="Upper bound of the delay, e.g. 3m." id={`${id}-max`}>
              <Input mono value={value.maxDuration} onChange={(e) => onChange({ ...value, maxDuration: e.target.value })} />
            </FormField>
            <Checkbox
              label="Refresh before retrying"
              description="Re-render from the latest revision when retrying."
              checked={value.refresh}
              onChange={(e) => onChange({ ...value, refresh: e.target.checked })}
            />
          </div>
        ) : null}
      </div>
    </fieldset>
  );
}
