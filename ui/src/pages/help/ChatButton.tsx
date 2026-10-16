import { useSettings } from '../../api/queries/resources';
import { buttonClassName, Tooltip } from '../../design';
import { IconExternal } from '../../design/icons';
import { chatLink } from './help';
import styles from './Help.module.css';

/**
 * help.chatUrl link (API-111). `floating` positions it at the bottom-right of the viewport, as
 * upstream's fixed chat button; the shell can mount it once. An invalid URL renders a disabled
 * control explaining why.
 */
export function ChatButton({ floating = false }: { floating?: boolean }) {
  const settings = useSettings();
  const chat = chatLink(settings.data);
  if (!chat) return null;
  const className = [buttonClassName('secondary', 'md'), floating ? styles.floatingChat : undefined].filter(Boolean).join(' ');
  if (!chat.href) {
    return (
      <Tooltip content="Invalid URL provided" describe={false}>
        <button type="button" className={className} aria-disabled="true" onClick={(e) => e.preventDefault()}>
          {chat.text}
          <span className="sr-only"> (unavailable: invalid URL provided)</span>
        </button>
      </Tooltip>
    );
  }
  return (
    <a className={className} href={chat.href} target="_blank" rel="noopener noreferrer">
      {chat.text}
      <IconExternal />
      <span className="sr-only"> (opens in a new tab)</span>
    </a>
  );
}
