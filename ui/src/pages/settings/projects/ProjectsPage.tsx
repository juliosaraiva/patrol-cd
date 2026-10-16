import { useId, useMemo, useState, type FormEvent } from 'react';
import { Link, useNavigate } from 'react-router';
import { useCreateProject } from '../../../api/hooks/settings';
import { useProjects } from '../../../api/queries/resources';
import type { AppProject } from '../../../api/types';
import {
  Button,
  Dialog,
  EmptyState,
  FormField,
  IconPlus,
  Input,
  PageHeader,
  SearchInput,
  Table,
  TextArea,
  useToast,
  type Column,
} from '../../../design';
import { useDocumentTitle } from '../../../lib/useDocumentTitle';
import { ErrorBanner } from '../../../shell/errors';
import { ActionError, ErrorSummary, LoadingBlock, useFormErrors } from '../common/feedback';
import styles from '../common/common.module.css';
import { validateObjectName } from '../common/validation';

const projectPath = (name: string) => `/settings/projects/${encodeURIComponent(name)}`;

function CreateProjectDialog({ onClose }: { onClose: () => void }) {
  const formId = useId();
  const navigate = useNavigate();
  const { toast } = useToast();
  const create = useCreateProject();
  const [name, setName] = useState('');
  const [description, setDescription] = useState('');
  const nameError = validateObjectName(name, 'Project name');
  const form = useFormErrors(nameError ? [{ id: 'project-new-name', message: nameError }] : []);

  const onSubmit = (event: FormEvent) => {
    event.preventDefault();
    if (!form.check()) return;
    const project: AppProject = {
      apiVersion: 'cd.patrol.example/v1alpha1',
      kind: 'AppProject',
      metadata: { name: name.trim() },
      spec: { description: description.trim() || undefined },
    };
    create.mutate(project, {
      onSuccess: (created) => {
        toast({ tone: 'success', title: 'Project created', description: created.metadata.name });
        onClose();
        void navigate(projectPath(created.metadata.name ?? name.trim()));
      },
    });
  };

  return (
    <Dialog
      open
      onClose={onClose}
      title="New project"
      description="A new project allows nothing until you add source repositories and destinations."
      footer={
        <>
          <Button onClick={onClose}>Cancel</Button>
          <Button variant="primary" type="submit" form={formId} loading={create.isPending}>
            Create project
          </Button>
        </>
      }
    >
      <form id={formId} className={styles.form} onSubmit={onSubmit} noValidate aria-label="New project">
        <ErrorSummary errors={form.errors} focusKey={form.attempt} />
        {create.error ? <ActionError error={create.error} action="create the project" /> : null}
        <FormField label="Name" id="project-new-name" required error={form.errorFor('project-new-name')} help="Lowercase letters, digits, “-” and “.”. Cannot be changed later.">
          <Input value={name} onChange={(e) => setName(e.target.value)} mono autoComplete="off" spellCheck={false} />
        </FormField>
        <FormField label="Description" id="project-new-description">
          <TextArea value={description} onChange={(e) => setDescription(e.target.value)} rows={3} />
        </FormField>
      </form>
    </Dialog>
  );
}

/** /settings/projects — projects list and creation (API-105, TEN-064). */
export function Component() {
  useDocumentTitle('Projects', 'Settings');
  const navigate = useNavigate();
  const projects = useProjects();
  const [creating, setCreating] = useState(false);
  const [search, setSearch] = useState('');
  const items = useMemo(() => projects.data?.items ?? [], [projects.data]);
  const filtered = useMemo(() => {
    const q = search.trim().toLowerCase();
    return q
      ? items.filter((p) => (p.metadata.name ?? '').toLowerCase().includes(q) || (p.spec.description ?? '').toLowerCase().includes(q))
      : items;
  }, [items, search]);

  const columns: Column<AppProject>[] = [
    {
      id: 'name',
      header: 'Name',
      width: '180px',
      cell: (p) => <Link to={projectPath(p.metadata.name ?? '')}>{p.metadata.name}</Link>,
      sortValue: (p) => p.metadata.name,
    },
    { id: 'description', header: 'Description', cell: (p) => <span title={p.spec.description}>{p.spec.description ?? '—'}</span>, sortValue: (p) => p.spec.description },
    { id: 'repos', header: 'Source repositories', width: '160px', align: 'end', cell: (p) => p.spec.sourceRepos?.length ?? 0, sortValue: (p) => p.spec.sourceRepos?.length ?? 0 },
    { id: 'destinations', header: 'Destinations', width: '120px', align: 'end', cell: (p) => p.spec.destinations?.length ?? 0, sortValue: (p) => p.spec.destinations?.length ?? 0 },
    { id: 'roles', header: 'Roles', width: '80px', align: 'end', cell: (p) => p.spec.roles?.length ?? 0, sortValue: (p) => p.spec.roles?.length ?? 0 },
    { id: 'windows', header: 'Sync windows', width: '120px', align: 'end', cell: (p) => p.spec.syncWindows?.length ?? 0, sortValue: (p) => p.spec.syncWindows?.length ?? 0 },
  ];

  let content;
  if (projects.isPending) content = <LoadingBlock label="Loading projects" />;
  else if (projects.isError) content = <ErrorBanner error={projects.error} onRetry={() => void projects.refetch()} />;
  else if (items.length === 0) {
    content = (
      <EmptyState
        title="No projects"
        action={
          <Button variant="primary" icon={<IconPlus />} onClick={() => setCreating(true)}>
            New project
          </Button>
        }
      >
        Every application belongs to a project, which limits its sources, destinations and who may act on it. Create a project for each
        team or environment.
      </EmptyState>
    );
  } else {
    content = (
      <>
        <div className={styles.toolbar}>
          <div className={styles.toolbarGrow}>
            <SearchInput value={search} onValueChange={setSearch} aria-label="Search projects" placeholder="Search by name or description" />
          </div>
        </div>
        <Table
          caption="Projects"
          className={styles.wideTable}
          columns={columns}
          rows={filtered}
          getRowKey={(p) => p.metadata.name ?? ''}
          defaultSort={{ columnId: 'name', direction: 'ascending' }}
          onRowActivate={(p) => void navigate(projectPath(p.metadata.name ?? ''))}
          empty={<EmptyState headingLevel={3} title="No projects match the search" action={<Button onClick={() => setSearch('')}>Clear search</Button>} />}
        />
      </>
    );
  }

  return (
    <>
      <PageHeader
        title="Projects"
        description="Projects group applications and restrict what they may deploy, where, and who may act on them."
        actions={
          items.length > 0 ? (
            <Button variant="primary" icon={<IconPlus />} onClick={() => setCreating(true)}>
              New project
            </Button>
          ) : undefined
        }
      />
      <div className={styles.stack}>{content}</div>
      {creating ? <CreateProjectDialog onClose={() => setCreating(false)} /> : null}
    </>
  );
}
