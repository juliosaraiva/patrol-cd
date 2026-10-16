# UI extension skeleton

A resource tab ("Backups") for `ops.example.com/BackupSchedule` objects, written in
TypeScript/React and built into one classic script.

- `src/index.tsx` — the component and its registration through
  `window.extensionsAPI.registerResourceExtension(component, group, kind, title, {icon})`.
- `src/host.ts` — types of the host globals (`window.extensionsAPI`, `window.React`,
  `window.ReactJSXRuntime`).
- `scripts/smoke.mjs` — loads the built bundle as the UI does and renders the tab.
- `src/shims/*` — esbuild aliases for `react` and `react/jsx-runtime` that resolve to the
  host's React 19 instance. Extensions must not bundle React: hooks would run against a
  second copy and fail.

```bash
pnpm install            # or npm install
pnpm typecheck          # tsc, strict mode
pnpm build              # -> dist/extension-backup-schedule.js (about 1.5 KB, no imports)
pnpm smoke              # loads the bundle with stub host globals and renders the tab
```

`react` and `react-dom` are dev dependencies for the smoke test only; the bundle uses the
host's React.

Install: copy `dist/extension-backup-schedule.js` into the extensions directory that the
Patrol CD server serves; the server concatenates every `extension*.js` file into
`extensions.js`, which the UI loads (the file name must start with `extension`). A runtime
error in one extension is caught and logged as `Extension <file> failed to load:`; it does
not break the UI or other extensions.

The same ABI as Argo CD 3.5 is supported, so extensions built for Argo CD load unmodified
as long as they treat React as external.
