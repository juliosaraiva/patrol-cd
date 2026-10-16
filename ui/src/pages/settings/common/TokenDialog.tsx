import { Banner, Button, CopyButton, Dialog, FormField, TextArea } from '../../../design';
import styles from './common.module.css';

/**
 * Shows a newly issued token exactly once. The value is not stored anywhere else in the UI;
 * closing the dialog discards it.
 */
export function TokenDialog({ token, title, onClose }: { token: string; title: string; onClose: () => void }) {
  return (
    <Dialog
      open
      onClose={onClose}
      title={title}
      description="Copy the token now. It is shown only once and cannot be retrieved later."
      closeOnBackdrop={false}
      footer={<Button variant="primary" onClick={onClose}>Done</Button>}
    >
      <div className={styles.tokenValue}>
        <Banner tone="warning" title="Store it securely">
          Anyone with this token can act with its permissions until it expires or is deleted.
        </Banner>
        <FormField label="Token">
          <TextArea value={token} readOnly mono rows={4} onFocus={(e) => e.currentTarget.select()} spellCheck={false} />
        </FormField>
        <div>
          <CopyButton value={token} label="Copy token" showLabel size="md" />
        </div>
      </div>
    </Dialog>
  );
}
