import { useState } from 'react';
import { Link } from 'react-router';
import { useGPGKeys } from '../../../api/queries/resources';
import type { AppProject } from '../../../api/types';
import { Badge, Banner, Button, FormField, IconClose, IconPlus, Mono, Select, Table } from '../../../design';
import { nonEmpty, StringListEditor, Suggestions } from '../common/editors';
import { useFormErrors, type FieldError } from '../common/feedback';
import styles from '../common/common.module.css';
import { ProjectSection, SectionDrawer, useProjectSave, type ProjectContext } from './shared';

type Policy = NonNullable<AppProject['spec']['sourceIntegrity']>['git']['policies'][number];
type Mode = Policy['gpg']['mode'];

const MODE_TEXT: Record<Mode, string> = {
  none: 'No verification',
  head: 'Verify the commit being synced',
  strict: 'Verify every commit in the history',
};

const KEY_ID = /^[0-9A-Fa-f]{16}$/;

interface PolicyForm {
  repos: string[];
  mode: Mode;
  keys: string[];
}

/** Signature keys and source integrity policies (API-105, TEN-064). */
export function IntegrityTab({ ctx }: { ctx: ProjectContext }) {
  const spec = ctx.project.spec;
  const gpg = useGPGKeys();
  const known = new Map((gpg.data?.items ?? []).map((k) => [k.keyID?.toUpperCase() ?? '', k]));
  const saver = useProjectSave(ctx);
  const form = useFormErrors();
  const [editing, setEditing] = useState<'keys' | 'policies' | null>(null);
  const [keys, setKeys] = useState<string[]>([]);
  const [policies, setPolicies] = useState<PolicyForm[]>([]);

  const signatureKeys = (spec.signatureKeys ?? []).map((k) => k.keyID);
  const sourcePolicies = spec.sourceIntegrity?.git.policies ?? [];
  const keyErrors = keys.map((k) => (KEY_ID.test(k.trim()) ? null : 'Use the 16-character hexadecimal key ID.'));
  const policyErrors = policies.map((p) => {
    if (nonEmpty(p.repos).length === 0) return 'Add at least one repository URL pattern.';
    if (p.mode !== 'none' && nonEmpty(p.keys).length === 0) return 'Add at least one key ID, or set the mode to no verification.';
    const bad = nonEmpty(p.keys).find((k) => !KEY_ID.test(k));
    return bad ? `"${bad}" is not a 16-character key ID.` : null;
  });

  const open = (which: 'keys' | 'policies') => {
    form.reset();
    saver.reset();
    setKeys([...signatureKeys]);
    setPolicies(sourcePolicies.map((p) => ({ repos: p.repos.map((r) => r.url), mode: p.gpg.mode, keys: [...p.gpg.keys] })));
    setEditing(which);
  };
  const close = () => setEditing(null);

  const submit = () => {
    if (editing === 'keys') {
      const errors: FieldError[] = keyErrors.flatMap((e, i) => (e ? [{ id: `sig-key-${i}-value`, message: `Key ${i + 1}: ${e}` }] : []));
      if (!form.check(errors)) return;
      saver.save((s) => ({ ...s, signatureKeys: nonEmpty(keys).map((keyID) => ({ keyID: keyID.toUpperCase() })) }), 'Signature keys updated', close);
      return;
    }
    const errors: FieldError[] = policyErrors.flatMap((e, i) => (e ? [{ id: `policy-${i}-repos-0-value`, message: `Policy ${i + 1}: ${e}` }] : []));
    if (!form.check(errors)) return;
    const next: Policy[] = policies.map((p) => ({
      repos: nonEmpty(p.repos).map((url) => ({ url })),
      gpg: { mode: p.mode, keys: nonEmpty(p.keys).map((k) => k.toUpperCase()) },
    }));
    saver.save(
      (s) => {
        const copy = { ...s };
        if (next.length === 0) delete copy.sourceIntegrity;
        else copy.sourceIntegrity = { git: { policies: next } };
        return copy;
      },
      'Source integrity policies updated',
      close,
    );
  };

  const keyIdSuggestions = [...known.keys()].filter(Boolean);
  const unknownKeys = (ids: readonly string[]) => ids.filter((id) => !known.has(id.toUpperCase()));

  return (
    <div className={styles.stack}>
      <ProjectSection
        id="project-signature-keys"
        title="Signature keys"
        description="When set, only commits signed by one of these GnuPG keys can be synced. Superseded by source integrity policies when both are set."
        onEdit={() => open('keys')}
        editLabel="Edit signature keys"
      >
        {signatureKeys.length === 0 ? (
          <p className={styles.muted}>No signature keys: commit signatures are not required.</p>
        ) : (
          <Table
            caption="Signature keys"
            className={styles.mediumTable}
            rows={signatureKeys}
            getRowKey={(k) => k}
            columns={[
              { id: 'id', header: 'Key ID', width: '190px', cell: (k) => <Mono>{k}</Mono> },
              {
                id: 'owner',
                header: 'Owner',
                cell: (k) => {
                  const key = known.get(k.toUpperCase());
                  return key ? (key.owner ?? '—') : <Badge tone="warning">Not in GnuPG keys</Badge>;
                },
              },
            ]}
          />
        )}
        {gpg.isSuccess && unknownKeys(signatureKeys).length > 0 ? (
          <Banner tone="warning" title="Some keys are not trusted yet">
            Commits signed with {unknownKeys(signatureKeys).join(', ')} fail verification until the public key is added under{' '}
            <Link to="/settings/gpgkeys">GnuPG keys</Link>.
          </Banner>
        ) : null}
      </ProjectSection>

      <ProjectSection
        id="project-source-integrity"
        title="Source integrity policies"
        description="Per-repository verification of Git commit signatures. The first policy whose repository pattern matches applies."
        onEdit={() => open('policies')}
        editLabel="Edit source integrity policies"
      >
        {sourcePolicies.length === 0 ? (
          <p className={styles.muted}>No policies.</p>
        ) : (
          <Table
            caption="Source integrity policies"
            className={styles.mediumTable}
            rows={sourcePolicies.map((p, i) => ({ p, i }))}
            getRowKey={(r) => String(r.i)}
            columns={[
              { id: 'repos', header: 'Repositories', cell: (r) => <Mono title={r.p.repos.map((x) => x.url).join(', ')}>{r.p.repos.map((x) => x.url).join(', ')}</Mono> },
              { id: 'mode', header: 'Verification', width: '240px', cell: (r) => MODE_TEXT[r.p.gpg.mode] },
              { id: 'keys', header: 'Keys', width: '200px', cell: (r) => (r.p.gpg.keys.length ? <Mono>{r.p.gpg.keys.join(', ')}</Mono> : '—') },
            ]}
          />
        )}
      </ProjectSection>

      {editing ? (
        <SectionDrawer
          title={editing === 'keys' ? 'Edit signature keys' : 'Edit source integrity policies'}
          onClose={close}
          onSubmit={submit}
          pending={saver.pending}
          error={saver.error}
          onReload={ctx.reload}
          errors={form.errors}
          attempt={form.attempt}
        >
          <Suggestions id="project-gpg-keys" values={keyIdSuggestions} />
          {editing === 'keys' ? (
            <StringListEditor
              legend="Signature keys"
              label="Key ID"
              noun="key"
              idPrefix="sig-key"
              values={keys}
              onChange={setKeys}
              list="project-gpg-keys"
              placeholder="4AEE18F83AFDEB23"
              errors={form.submitted ? keyErrors : undefined}
              help="16-character key IDs of keys added under GnuPG keys. Suggestions list the known keys."
              emptyText="No keys: signatures are not required."
            />
          ) : (
            <>
              {policies.map((p, i) => (
                <fieldset key={i} className={styles.details}>
                  <legend className={styles.legend}>{`Policy ${i + 1}`}</legend>
                  <div className={styles.stackTight}>
                    {form.submitted && policyErrors[i] ? <p className={styles.rowError}>{policyErrors[i]}</p> : null}
                    <StringListEditor
                      legend="Repository URL patterns"
                      label="Repository URL pattern"
                      noun="repository"
                      idPrefix={`policy-${i}-repos`}
                      values={p.repos}
                      onChange={(repos) => setPolicies(policies.map((x, j) => (j === i ? { ...x, repos } : x)))}
                      placeholder="https://git.example.com/team/*"
                    />
                    <FormField label="Verification" id={`policy-${i}-mode`}>
                      <Select
                        value={p.mode}
                        onChange={(e) => setPolicies(policies.map((x, j) => (j === i ? { ...x, mode: e.target.value as Mode } : x)))}
                        options={(Object.keys(MODE_TEXT) as Mode[]).map((m) => ({ value: m, label: `${m}: ${MODE_TEXT[m]}` }))}
                      />
                    </FormField>
                    <StringListEditor
                      legend="Trusted keys"
                      label="Key ID"
                      noun="key"
                      idPrefix={`policy-${i}-keys`}
                      values={p.keys}
                      onChange={(k) => setPolicies(policies.map((x, j) => (j === i ? { ...x, keys: k } : x)))}
                      list="project-gpg-keys"
                      placeholder="4AEE18F83AFDEB23"
                    />
                    <div>
                      <Button size="sm" variant="ghost" icon={<IconClose />} onClick={() => setPolicies(policies.filter((_, j) => j !== i))}>
                        {`Remove policy ${i + 1}`}
                      </Button>
                    </div>
                  </div>
                </fieldset>
              ))}
              <div>
                <Button size="sm" icon={<IconPlus />} onClick={() => setPolicies([...policies, { repos: [''], mode: 'head', keys: [] }])}>
                  Add policy
                </Button>
              </div>
            </>
          )}
        </SectionDrawer>
      ) : null}
    </div>
  );
}
