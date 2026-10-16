import type { ReactNode } from 'react';
import { Badge, CodeBlock, KeyValueList, Mono, Table, type Column, type KeyValueItem } from '../../design';
import { toYAML } from './diff';
import { formatSelector, GENERATOR_LABELS, generatorKinds, type Generator } from './model';
import { pullRequestSource } from './previews';
import styles from './ApplicationSets.module.css';

const MAX_LIST_ROWS = 50;

type Item = KeyValueItem | false | undefined | null;

function kv(items: Item[]): KeyValueItem[] {
  return items.filter((i): i is KeyValueItem => Boolean(i));
}

function item(key: string, label: string, value: ReactNode | undefined): Item {
  if (value === undefined || value === null || value === '') return null;
  return { key, label, value };
}

function mono(value: string | undefined): ReactNode | undefined {
  return value ? <Mono>{value}</Mono> : undefined;
}

function seconds(value: number | undefined): string | undefined {
  return value === undefined ? undefined : `${value} s`;
}

function providerRef(ref: { name?: string } | undefined): ReactNode | undefined {
  return ref?.name ? (
    <>
      SCMProvider <Mono>{ref.name}</Mono>
    </>
  ) : undefined;
}

type SCMGenerator = NonNullable<Generator['scmProvider']>;

function scmProviderText(s: SCMGenerator): string {
  if (s.github) return `GitHub organization ${s.github.organization}`;
  if (s.gitlab) return `GitLab group ${s.gitlab.group}`;
  if (s.gitea) return `Gitea owner ${s.gitea.owner}`;
  if (s.bitbucket) return `Bitbucket Cloud workspace ${s.bitbucket.owner}`;
  if (s.bitbucketServer) return `Bitbucket Data Center project ${s.bitbucketServer.project}`;
  if (s.azureDevOps) return `Azure DevOps ${s.azureDevOps.organization}/${s.azureDevOps.teamProject}`;
  if (s.awsCodeCommit) return `AWS CodeCommit${s.awsCodeCommit.region ? ` in ${s.awsCodeCommit.region}` : ''}`;
  return 'Provider not recognized';
}

function ListElements({ elements }: { elements: Record<string, unknown>[] }) {
  const keys = [...new Set(elements.flatMap((e) => Object.keys(e)))].slice(0, 8);
  const rows = elements.slice(0, MAX_LIST_ROWS).map((e, i) => ({ i, e }));
  const columns: Column<(typeof rows)[number]>[] = keys.map((k) => ({
    id: k,
    header: k,
    cell: (r) => {
      const v = r.e[k];
      return v === undefined ? '' : <Mono>{typeof v === 'string' ? v : JSON.stringify(v)}</Mono>;
    },
  }));
  return (
    <>
      <Table caption="List elements" columns={columns} rows={rows} getRowKey={(r) => String(r.i)} />
      {elements.length > MAX_LIST_ROWS ? <p className={styles.muted}>{`Showing ${MAX_LIST_ROWS} of ${elements.length} elements; see the manifest for all.`}</p> : null}
    </>
  );
}

function GeneratorBody({ gen, depth }: { gen: Generator; depth: number }) {
  const kinds = generatorKinds(gen);
  const kind = kinds[0];
  const selector = gen.selector ? formatSelector(gen.selector) : '';
  const post = selector ? <p className={styles.muted}>{`Post-selector: only parameter sets matching ${selector}`}</p> : null;
  if (gen.list) {
    const elements = (gen.list.elements ?? []) as Record<string, unknown>[];
    return (
      <>
        <p className={styles.muted}>{gen.list.elementsYaml ? 'Elements from a YAML string (elementsYaml).' : `${elements.length} element${elements.length === 1 ? '' : 's'}, one Application each.`}</p>
        {elements.length > 0 ? <ListElements elements={elements} /> : null}
        {gen.list.elementsYaml ? <CodeBlock title="elementsYaml" code={gen.list.elementsYaml} maxHeight={200} /> : null}
        {post}
      </>
    );
  }
  if (gen.clusters) {
    const c = gen.clusters;
    const sel = formatSelector(c.selector);
    return (
      <>
        <KeyValueList
          items={kv([
            item('selector', 'Clusters', sel ? <Mono>{sel}</Mono> : 'Every registered cluster (and the local cluster when registered)'),
            item('values', 'Extra values', c.values && Object.keys(c.values).length > 0 ? <Mono>{Object.entries(c.values).map(([k, v]) => `${k}=${v}`).join(', ')}</Mono> : undefined),
            item('flat', 'Flat list', c.flatList ? 'Yes: one parameter set listing every cluster' : undefined),
          ])}
        />
        {post}
      </>
    );
  }
  if (gen.git) {
    const g = gen.git;
    const paths = [...(g.directories ?? []).map((d) => ({ ...d, kind: 'directory' })), ...(g.files ?? []).map((f) => ({ ...f, kind: 'file' }))];
    return (
      <>
        <KeyValueList
          items={kv([
            item('repo', 'Repository', mono(g.repoURL)),
            item('revision', 'Revision', mono(g.revision || 'HEAD')),
            item('mode', 'Mode', g.files && g.files.length > 0 ? 'Files: one parameter set per matching JSON/YAML file' : 'Directories: one parameter set per matching directory'),
            item('prefix', 'Parameter prefix', mono(g.pathParamPrefix)),
            item('requeue', 'Polling interval', seconds(g.requeueAfterSeconds)),
          ])}
        />
        {paths.length > 0 ? (
          <ul className={styles.plainList} aria-label="Git paths">
            {paths.map((p, i) => (
              <li key={i}>
                {p.exclude ? 'Exclude ' : 'Include '}
                {p.kind} <Mono>{p.path}</Mono>
              </li>
            ))}
          </ul>
        ) : null}
        {post}
      </>
    );
  }
  if (gen.scmProvider) {
    const s = gen.scmProvider;
    const provider = scmProviderText(s);
    const ref =
      s.github?.providerRef ?? s.gitlab?.providerRef ?? s.gitea?.providerRef ?? s.bitbucket?.providerRef ?? s.bitbucketServer?.providerRef ?? s.azureDevOps?.providerRef ?? s.awsCodeCommit?.providerRef;
    const allBranches =
      s.github?.allBranches ?? s.gitlab?.allBranches ?? s.gitea?.allBranches ?? s.bitbucket?.allBranches ?? s.bitbucketServer?.allBranches ?? s.azureDevOps?.allBranches ?? s.awsCodeCommit?.allBranches;
    const filters = (s.filters ?? []).map((f) =>
      [
        f.repositoryMatch && `repository ~ ${f.repositoryMatch}`,
        f.branchMatch && `branch ~ ${f.branchMatch}`,
        f.labelMatch && `label ~ ${f.labelMatch}`,
        f.pathsExist?.length ? `has ${f.pathsExist.join(', ')}` : '',
        f.pathsDoNotExist?.length ? `lacks ${f.pathsDoNotExist.join(', ')}` : '',
      ]
        .filter(Boolean)
        .join('; '),
    );
    return (
      <>
        <KeyValueList
          items={kv([
            item('provider', 'Repositories of', provider),
            item('ref', 'Credentials', providerRef(ref)),
            item('branches', 'Branches', allBranches ? 'Every branch' : 'Default branch only'),
            item('clone', 'Clone protocol', s.cloneProtocol),
            item('filters', 'Filters (any matches)', filters.length > 0 ? filters.join(' | ') : undefined),
            item('requeue', 'Polling interval', seconds(s.requeueAfterSeconds)),
          ])}
        />
        {post}
      </>
    );
  }
  if (gen.pullRequest) {
    const src = pullRequestSource(gen.pullRequest);
    const p = gen.pullRequest;
    const ref = p.github?.providerRef ?? p.gitlab?.providerRef ?? p.gitea?.providerRef ?? p.bitbucket?.providerRef ?? p.bitbucketServer?.providerRef ?? p.azuredevops?.providerRef;
    return (
      <>
        <KeyValueList
          items={kv([
            item('provider', 'Pull requests of', <>{src.provider} {src.repository ? <Mono>{src.repository}</Mono> : null}</>),
            item('ref', 'Credentials', providerRef(ref)),
            item('labels', 'Required labels', src.labels.length > 0 ? src.labels.join(', ') : undefined),
            item('state', 'State', p.gitlab?.pullRequestState ? `GitLab state ${p.gitlab.pullRequestState}` : 'Open pull requests'),
            item('filters', 'Filters (any matches)', src.filters.length > 0 ? src.filters.join(' | ') : undefined),
            item('requeue', 'Polling interval', `${src.requeueAfterSeconds} s${p.requeueAfterSeconds === undefined ? ' (default)' : ''}`),
          ])}
        />
        {post}
      </>
    );
  }
  if (gen.clusterDecisionResource) {
    const c = gen.clusterDecisionResource;
    return (
      <>
        <KeyValueList
          items={kv([
            item('cm', 'Duck-type ConfigMap', mono(c.configMapRef)),
            item('name', 'Resource name', mono(c.name)),
            item('selector', 'Label selector', c.labelSelector ? <Mono>{formatSelector(c.labelSelector)}</Mono> : undefined),
            item('requeue', 'Polling interval', seconds(c.requeueAfterSeconds)),
          ])}
        />
        {post}
      </>
    );
  }
  if (gen.plugin) {
    const p = gen.plugin;
    return (
      <>
        <KeyValueList
          items={kv([
            item('cm', 'Plugin ConfigMap', mono(p.configMapRef.name)),
            item('requeue', 'Polling interval', seconds(p.requeueAfterSeconds)),
          ])}
        />
        {p.input?.parameters ? <CodeBlock title="Input parameters" code={toYAML(p.input.parameters)} maxHeight={200} /> : null}
        {post}
      </>
    );
  }
  if (gen.matrix || gen.merge) {
    const nested = ((gen.matrix?.generators ?? gen.merge?.generators ?? []) as Generator[]).slice();
    return (
      <>
        <p className={styles.muted}>
          {gen.matrix
            ? 'Every combination of the parameter sets of its two generators (later generators can use parameters of earlier ones).'
            : `Parameter sets merged by ${(gen.merge?.mergeKeys ?? []).join(', ') || 'their merge keys'}; later generators override matching sets of the first.`}
        </p>
        <GeneratorList generators={nested} depth={depth + 1} />
        {post}
      </>
    );
  }
  return <p className={styles.muted}>{kind ? `${GENERATOR_LABELS[kind] ?? kind} generator.` : 'Empty generator: the controller reports an error for it.'}</p>;
}

function hasOwnTemplate(gen: Generator): boolean {
  return generatorKinds(gen).some((k) => {
    const tpl = (gen as Record<string, { template?: { metadata?: object; spec?: object } } | undefined>)[k]?.template;
    return tpl !== undefined && (Object.keys(tpl.metadata ?? {}).length > 0 || Object.keys(tpl.spec ?? {}).length > 0);
  });
}

/** Readable rendering of ApplicationSet generators (nested for matrix and merge). */
export function GeneratorList({ generators, depth = 0 }: { generators: readonly Generator[]; depth?: number }) {
  if (generators.length === 0) return <p className={styles.muted}>No generators: the ApplicationSet generates nothing.</p>;
  const Heading = depth === 0 ? 'h3' : 'h4';
  return (
    <ol className={depth === 0 ? styles.generators : styles.nestedGenerators}>
      {generators.map((gen, index) => {
        const kinds = generatorKinds(gen);
        return (
          <li key={index} className={styles.generator}>
            <Heading className={styles.generatorTitle}>
              {`${depth === 0 ? 'Generator' : 'Child generator'} ${index + 1}: ${kinds.map((k) => GENERATOR_LABELS[k] ?? k).join(', ') || 'empty'}`}
              {hasOwnTemplate(gen) ? <Badge>Overrides the template</Badge> : null}
            </Heading>
            <GeneratorBody gen={gen} depth={depth} />
          </li>
        );
      })}
    </ol>
  );
}
