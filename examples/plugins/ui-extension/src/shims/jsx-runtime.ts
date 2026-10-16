/** Build-time stand-in for "react/jsx-runtime" (esbuild --alias), used by the automatic JSX
 * transform: the host's runtime. */
const runtime = window.ReactJSXRuntime;

export const { Fragment, jsx, jsxs } = runtime;
