import { useId, type FormEvent, type ReactNode } from 'react';
import { useUpdateProject } from '../../../api/hooks/settings';
import type { AppProject } from '../../../api/types';
import { Button, Drawer, useToast } from '../../../design';
import { ActionError, ErrorSummary, type FieldError } from '../common/feedback';
import styles from '../common/common.module.css';

export type ProjectSpec = AppProject['spec'];

export interface ProjectContext {
  project: AppProject;
  /** Refetches the project (after a 409 conflict, to reapply edits on the latest version). */
  reload: () => void;
}

/**
 * Saves a change to the project: the whole object is PUT with the resourceVersion it was loaded
 * with, so a concurrent edit fails with 409 instead of being overwritten.
 */
export function useProjectSave({ project }: ProjectContext) {
  const update = useUpdateProject();
  const { toast } = useToast();
  return {
    pending: update.isPending,
    error: update.error,
    reset: update.reset,
    save(change: (spec: ProjectSpec) => ProjectSpec, success: string, onDone: () => void, metadata?: Partial<AppProject['metadata']>) {
      const next: AppProject = {
        ...project,
        metadata: { ...project.metadata, ...metadata },
        spec: change(structuredClone(project.spec)),
      };
      update.mutate(next, {
        onSuccess: () => {
          toast({ tone: 'success', title: success, description: `Project ${project.metadata.name ?? ''}` });
          onDone();
        },
      });
    },
  };
}

/** Drawer holding the editor of one project section. */
export function SectionDrawer({
  title,
  description,
  onClose,
  onSubmit,
  pending,
  error,
  onReload,
  errors,
  attempt,
  children,
  extra,
  submitLabel = 'Save',
}: {
  title: string;
  description?: ReactNode;
  onClose: () => void;
  onSubmit: () => void;
  pending: boolean;
  error: unknown;
  onReload: () => void;
  errors: readonly FieldError[];
  attempt: number;
  children: ReactNode;
  /** Content after the form (e.g. a panel with its own form; forms cannot nest). */
  extra?: ReactNode;
  submitLabel?: string;
}) {
  const formId = useId();
  return (
    <Drawer
      open
      onClose={onClose}
      width="680px"
      title={title}
      description={description}
      footer={
        <>
          <Button onClick={onClose}>Cancel</Button>
          <Button variant="primary" type="submit" form={formId} loading={pending}>
            {submitLabel}
          </Button>
        </>
      }
    >
      <div className={styles.stack}>
      <form
        id={formId}
        className={styles.form}
        noValidate
        aria-label={title}
        onSubmit={(e: FormEvent) => {
          e.preventDefault();
          onSubmit();
        }}
      >
        <ErrorSummary errors={errors} focusKey={attempt} />
        {error ? <ActionError error={error} action="update the project" onReload={onReload} /> : null}
        {children}
      </form>
      {extra}
      </div>
    </Drawer>
  );
}

/** A titled block on a project tab with an optional Edit action. */
export function ProjectSection({
  id,
  title,
  description,
  onEdit,
  editLabel,
  children,
}: {
  id: string;
  title: string;
  description?: ReactNode;
  onEdit?: () => void;
  editLabel?: string;
  children: ReactNode;
}) {
  return (
    <section className={styles.section} aria-labelledby={id}>
      <div className={styles.sectionHeader}>
        <div className={styles.sectionTitle}>
          <h3 id={id}>{title}</h3>
          {description ? <p className={styles.small}>{description}</p> : null}
        </div>
        {onEdit ? (
          <Button size="sm" onClick={onEdit} aria-label={editLabel ?? `Edit ${title.toLowerCase()}`}>
            Edit
          </Button>
        ) : null}
      </div>
      {children}
    </section>
  );
}

/** Seconds since the epoch → Date (JWT iat/exp). */
export function fromUnix(seconds: number | undefined): Date | undefined {
  return seconds ? new Date(seconds * 1000) : undefined;
}
