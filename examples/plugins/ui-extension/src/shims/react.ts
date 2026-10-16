/** Build-time stand-in for "react" (esbuild --alias): the host's React instance, so hooks
 * and context work with the Patrol CD UI's renderer. Re-export what the extension uses. */
const React = window.React;

export default React;
export const { Fragment, useMemo, useState } = React;
