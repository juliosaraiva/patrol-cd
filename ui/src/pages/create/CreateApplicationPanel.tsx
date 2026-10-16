import { useId, useRef } from 'react';
import type { Application } from '../../api/types';
import { Drawer } from '../../design';
import { ApplicationCreateForm } from './ApplicationCreateForm';

export interface CreateApplicationPanelProps {
  open: boolean;
  onClose: () => void;
  /** Prefill, e.g. from a repository's "Create application" action. */
  initial?: { repoURL?: string; project?: string } | undefined;
  onCreated?: ((app: Application) => void) | undefined;
}

/**
 * Slide-in creation wizard (API-091), opened from the applications list (?new=1) or a
 * repository. The applications area mounts it; the form owns its submit and cancel buttons.
 */
export function CreateApplicationPanel({ open, onClose, initial, onCreated }: CreateApplicationPanelProps) {
  const formId = useId();
  const nameRef = useRef<HTMLInputElement>(null);
  return (
    <Drawer
      open={open}
      onClose={onClose}
      title="New application"
      description="Deploy manifests from a repository to a cluster and namespace."
      width="min(920px, 100vw)"
      initialFocusRef={nameRef}
    >
      {open ? <ApplicationCreateForm formId={formId} initial={initial} onCancel={onClose} onCreated={onCreated} nameInputRef={nameRef} /> : null}
    </Drawer>
  );
}
