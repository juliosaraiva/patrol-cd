import { useState } from 'react';
import { Link, useNavigate, useParams } from 'react-router';
import { useDeleteProject } from '../../../api/hooks/settings';
import { isApiError } from '../../../api/errors';
import { useApplications } from '../../../api/queries/applications';
import { useDetailedProject, useProject } from '../../../api/queries/resources';
import { Banner, Button, ButtonLink, EmptyState, Mono, PageHeader, Tabs, useToast } from '../../../design';
import { useDocumentTitle } from '../../../lib/useDocumentTitle';
import { ErrorBanner } from '../../../shell/errors';
import { useTabParam } from '../common/editors';
import { ConfirmDialog, LoadingBlock } from '../common/feedback';
import styles from '../common/common.module.css';
import { EventsTab } from './EventsTab';
import { IntegrityTab } from './IntegrityTab';
import { OrphanedTab } from './OrphanedTab';
import { ResourcesTab } from './ResourcesTab';
import { RolesTab } from './RolesTab';
import { SummaryTab } from './SummaryTab';
import { SyncWindowsTab } from './SyncWindowsTab';

const TABS = ['summary', 'resources', 'roles', 'windows', 'integrity', 'orphaned', 'events'] as const;

function DeleteProjectDialog({ name, open, onClose }: { name: string; open: boolean; onClose: () => void }) {
  const navigate = useNavigate();
  const { toast } = useToast();
  const remove = useDeleteProject();
  const apps = useApplications({ projects: [name] }, { watch: false, enabled: open });
  const using = (apps.data?.items ?? []).filter((a) => a.spec.project === name);
  return (
    <ConfirmDialog
      open={open}
      onClose={() => {
        remove.reset();
        onClose();
      }}
      title="Delete project?"
      confirmLabel="Delete project"
      action="delete the project"
      confirmText={name}
      busy={remove.isPending}
      error={remove.error}
      onConfirm={() =>
        remove.mutate(name, {
          onSuccess: () => {
            toast({ tone: 'success', title: 'Project deleted', description: name });
            onClose();
            void navigate('/settings/projects');
          },
        })
      }
    >
      {apps.isPending ? <p className={styles.muted}>Checking which applications use this project…</p> : null}
      {using.length > 0 ? (
        <Banner tone="warning" title={`${using.length} application${using.length === 1 ? ' uses' : 's use'} this project`}>
          <p>The server refuses to delete a project that applications still reference. Move them to another project or delete them first:</p>
          <ul>
            {using.slice(0, 5).map((a) => (
              <li key={`${a.metadata.namespace ?? ''}/${a.metadata.name ?? ''}`}>
                <Link to={`/applications/${encodeURIComponent(a.metadata.name ?? '')}`}>{a.metadata.name}</Link>
              </li>
            ))}
            {using.length > 5 ? <li>and {using.length - 5} more</li> : null}
          </ul>
        </Banner>
      ) : null}
      <p>
        Roles, tokens and sync windows of <Mono>{name}</Mono> are deleted with it. Repositories and clusters scoped to the project lose
        their scope.
      </p>
    </ConfirmDialog>
  );
}

/** /settings/projects/:name — project details (API-105, TEN-064). */
export function Component() {
  const { name = '' } = useParams();
  useDocumentTitle(name, 'Projects');
  const project = useProject(name);
  const detailed = useDetailedProject(name);
  const [tab, setTab] = useTabParam(TABS, 'summary');
  const [deleting, setDeleting] = useState(false);

  const header = (
    <PageHeader
      title={name}
      description={project.data?.spec.description ?? 'Project'}
      actions={
        project.data ? (
          <Button variant="danger" onClick={() => setDeleting(true)}>
            Delete project
          </Button>
        ) : undefined
      }
    />
  );

  if (project.isPending) {
    return (
      <>
        {header}
        <LoadingBlock label="Loading project" />
      </>
    );
  }
  if (!project.data) {
    const notFound = isApiError(project.error) && project.error.status === 404;
    return (
      <>
        {header}
        {notFound ? (
          <EmptyState title="Project not found" action={<ButtonLink to="/settings/projects">Back to projects</ButtonLink>}>
            No project named <Mono>{name}</Mono> exists. It may have been deleted.
          </EmptyState>
        ) : (
          <ErrorBanner error={project.error} onRetry={() => void project.refetch()} />
        )}
      </>
    );
  }

  const ctx = { project: project.data, reload: () => void project.refetch() };
  return (
    <>
      {header}
      {project.isError ? <ErrorBanner error={project.error} title="Showing the last loaded version" onRetry={() => void project.refetch()} /> : null}
      <Tabs
        label="Project sections"
        value={tab}
        onValueChange={setTab}
        items={[
          { id: 'summary', label: 'Summary', content: <SummaryTab ctx={ctx} detailed={detailed.data} /> },
          { id: 'resources', label: 'Resources', content: <ResourcesTab ctx={ctx} /> },
          { id: 'roles', label: 'Roles', content: <RolesTab ctx={ctx} /> },
          { id: 'windows', label: 'Sync windows', content: <SyncWindowsTab ctx={ctx} /> },
          { id: 'integrity', label: 'Signature keys', content: <IntegrityTab ctx={ctx} /> },
          { id: 'orphaned', label: 'Orphaned resources', content: <OrphanedTab ctx={ctx} /> },
          { id: 'events', label: 'Events', content: <EventsTab name={name} /> },
        ]}
      />
      {deleting ? <DeleteProjectDialog name={name} open={deleting} onClose={() => setDeleting(false)} /> : null}
    </>
  );
}
