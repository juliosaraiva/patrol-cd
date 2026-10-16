import { Component, type ErrorInfo, type ReactNode } from 'react';
import { Banner } from '../design';

interface Props {
  /** Shown to the user when the extension throws while rendering. */
  message: string;
  children: ReactNode;
}

interface State {
  error: unknown;
}

/**
 * Error boundary around every extension component: a render error in third-party code shows a
 * contained message instead of taking down the page (API-121).
 */
export class ExtensionBoundary extends Component<Props, State> {
  override state: State = { error: null };

  static getDerivedStateFromError(error: unknown): State {
    return { error: error ?? new Error('unknown error') };
  }

  override componentDidCatch(error: unknown, info: ErrorInfo): void {
    console.error(`${this.props.message}:`, error, info.componentStack);
  }

  override render(): ReactNode {
    if (this.state.error) {
      return (
        <Banner tone="error" title={this.props.message}>
          The extension failed while rendering. Reload the page; if it keeps failing, ask the operator who installed the extension to check
          the browser console.
        </Banner>
      );
    }
    return this.props.children;
  }
}
