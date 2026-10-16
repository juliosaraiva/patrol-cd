/**
 * Full-page navigations, behind an object so tests can replace them (jsdom cannot navigate).
 */
export const browser = {
  assign(href: string): void {
    window.location.assign(href);
  },
  reload(): void {
    window.location.reload();
  },
};
