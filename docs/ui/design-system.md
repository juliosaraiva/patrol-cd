# Patrol CD design system

Status: foundation implemented (`ui/src/design/`). Component conventions for page authors live
in [`docs/architecture/components/ui.md`](../architecture/components/ui.md).

The UI is a dense operations console: people scan many applications, compare states and act
on them. The system optimizes for legibility at small sizes, unambiguous status, and full
keyboard and screen-reader support. No CSS framework is used: CSS modules plus design tokens
exposed as CSS custom properties.

A development-only gallery of every component renders at `/dev/design` under `pnpm dev`
(`pnpm dev:mock` without a server). It is excluded from production builds.

## Principles

1. **Neutral surfaces, one accent.** Pure white (light) and near-black (dark) backgrounds,
   neutral grays, and a single deep-teal accent for primary actions, focus and selection.
   No cream or off-white surfaces, no gradients, no glassmorphism or backdrop blur.
2. **Status never relies on color alone.** Every status has a text label and a distinct
   shape; color is the third cue (see [Status](#status)).
3. **Compact but readable.** 13px/14px body type, 32px controls, 36px table rows.
4. **Flat and quiet.** 1px borders, 4px maximum radius for controls (no pill buttons),
   shadows only on floating layers (menus, dialogs, toasts).
5. **Motion is optional.** Durations are ≤180 ms and `prefers-reduced-motion` disables all
   transitions and animations; nothing depends on animation to convey meaning.
6. **Monospace means "identifier".** `<Mono>`/`<Code>`/`<CodeBlock>` only, for revisions,
   resource names, URLs, commands and manifests; never for labels or headings. No italic
   heading accents.

Several of these are enforced by `src/design/tokens.test.ts`: AA contrast for every
documented token pair in both themes, identical dark palettes for the OS preference and the
explicit override, white/near-black backgrounds, no `gradient(` or `backdrop-filter` in any
stylesheet under `src/`, no `border-radius` above 4px (or 50%/999px), and zeroed motion
tokens under reduced motion.

## Tokens (`src/design/tokens.css`)

### Color

Light values are defined on `:root`; dark values apply under
`@media (prefers-color-scheme: dark) { :root:not([data-theme='light']) }` and under
`:root[data-theme='dark']`.

| Token | Use |
|-------|-----|
| `--color-bg`, `--color-surface` | Page and panel background (`#ffffff` / `#0f1114`, `#16191d`) |
| `--color-surface-subtle`, `--color-surface-hover`, `--color-surface-selected` | Table header and code background, hover, selected rows |
| `--color-border`, `--color-border-strong` | Dividers; control borders (≥3:1 against the surface) |
| `--color-text`, `--color-text-muted`, `--color-text-placeholder`, `--color-text-inverse` | Body, secondary, placeholder, tooltip text |
| `--color-accent`, `--color-accent-hover`, `--color-accent-subtle`, `--color-on-accent` | Primary buttons, links, focus ring (`--color-focus`), selection |
| `--color-danger`, `--color-warning`, `--color-success`, `--color-info` (+ `-subtle`) | Destructive actions, banners, status glyphs |
| `--color-suspended`, `--color-unknown` | Status glyphs |
| `--status-*` | Semantic aliases used by status components |

Measured contrast (WCAG 2.2; computed from the token values, asserted by `tokens.test.ts`):

| Pair | Light | Ratio | Dark | Ratio |
|------|-------|-------|------|-------|
| `--color-text` on `--color-bg` | #16191d / #ffffff | 17.63 | #e7e9ec / #0f1114 | 15.55 |
| `--color-text-muted` on `--color-bg` | #545b66 / #ffffff | 6.85 | #a3aab4 / #0f1114 | 8.07 |
| `--color-text-placeholder` on `--color-surface` | #6a717c / #ffffff | 4.92 | #8d949e / #16191d | 5.76 |
| `--color-accent` on `--color-bg` | #0b6b66 / #ffffff | 6.35 | #45bdb2 / #0f1114 | 8.26 |
| `--color-on-accent` on `--color-accent` | #ffffff / #0b6b66 | 6.35 | #062321 / #45bdb2 | 7.22 |
| `--color-on-danger` on `--color-danger` | #ffffff / #b42318 | 6.57 | #1a0705 / #f2887d | 7.98 |
| `--color-danger` on `--color-surface` | #b42318 / #ffffff | 6.57 | #f2887d / #16191d | 7.22 |
| `--color-warning` on `--color-surface` | #8f5500 / #ffffff | 6.06 | #e2a640 / #16191d | 8.20 |
| `--color-success` on `--color-surface` | #1b7339 / #ffffff | 5.90 | #5fc488 / #16191d | 8.18 |
| `--color-info` on `--color-surface` | #1d5cb8 / #ffffff | 6.41 | #7eaaf2 / #16191d | 7.49 |
| `--color-suspended` on `--color-surface` | #5f4fae / #ffffff | 6.53 | #a99cf0 / #16191d | 7.31 |
| `--color-unknown` on `--color-surface` | #5f6670 / #ffffff | 5.80 | #a3aab4 / #16191d | 7.53 |
| `--color-border-strong` on `--color-surface` | #868d98 / #ffffff | 3.35 | #6f7782 / #16191d | 3.89 |

### Typography

| Token | Value | Use |
|-------|-------|-----|
| `--font-sans` | Inter, ui-sans-serif, system-ui, … | All UI text (Inter when installed; no web font is downloaded) |
| `--font-mono` | ui-monospace, SFMono-Regular, Menlo, … | Identifiers and code only |
| `--text-xs` / `--text-sm` / `--text-md` | 12 / 13 / 14px | Metadata, tables and controls, body |
| `--text-lg` / `--text-xl` / `--text-2xl` | 16 / 20 / 24px | Section titles, page titles |
| `--weight-regular/medium/semibold` | 400 / 500 / 600 | Body, controls, headings |

Tabular numerals are on by default (`font-feature-settings: 'tnum'`).

### Space, shape, layers, motion

- Space: 4px grid, `--space-1` (2px) … `--space-11` (48px). Page gutters are `--space-8`
  (24px) on desktop and `--space-6` (16px) below 768px.
- Shape: `--radius-sm` 2px, `--radius-md` 4px (the maximum), `--border-width` 1px; control
  heights 28px (`sm`) and 32px (`md`).
- Layers: `--z-sticky` 10 < `--z-nav` 50 < `--z-dropdown` 100 < `--z-overlay` 200 <
  `--z-toast` 300 < `--z-tooltip` 400.
- Motion: `--duration-fast` 120ms, `--duration-normal` 180ms, `--easing-standard`; both
  durations are 0 under `prefers-reduced-motion`, and `global.css` also clamps every
  animation and transition there.

## Theming

`src/design/theme.ts` keeps the preference (`system` | `light` | `dark`) in `localStorage`
under `patrol.theme` (all storage access goes through `src/lib/storage.ts`, which never
throws). `initTheme()` runs before the first render; an explicit choice sets
`<html data-theme="light|dark">`, `system` removes the attribute so the media query applies.
`useTheme()` exposes the preference and the resolved theme and reacts to OS changes. The top
bar theme menu and Settings → Appearance share this store. No inline script is used, so the
page works under a strict `script-src 'self'` CSP.

## Icons

`src/design/icons/index.tsx`: inline SVG components authored for Patrol (16×16 grid, 1.5px
strokes, `currentColor`). Icons are `aria-hidden` unless given a `title`. Icon-only controls
must use `IconButton`, whose type requires `aria-label`. `PatrolMark` is the product mark.

## Components (`src/design/components/`)

| Component | Notes |
|-----------|-------|
| `Button`, `ButtonLink`, `IconButton` | Variants `primary` (one per view), `secondary`, `danger`, `ghost`; sizes `sm`/`md`. `loading` sets `aria-busy` and ignores clicks. `ButtonLink` is a router link styled as a button (navigation, not action). `IconButton` requires `aria-label`. |
| `Spinner` | `role="status"` with a label; static arc under reduced motion. |
| `FormField` + `Input`, `TextArea`, `Select` | `FormField` renders the label, help and error and injects `id`, `aria-describedby`, `aria-invalid`, `required` into the control. `mono` for identifiers. `Select` is a native `<select>`. |
| `Checkbox`, `Radio`, `RadioGroup`, `Switch` | Native inputs (`accent-color`); indeterminate checkboxes report `aria-checked="mixed"`; `RadioGroup` is a fieldset with legend; `Switch` is `role="switch"`. |
| `Tabs` | APG tabs, automatic activation, roving tabindex, ←/→/Home/End, disabled tabs skipped; panel is focusable. |
| `Table` | Sortable headers (`aria-sort`, natural sort), optional selection (select-all with indeterminate state), one focusable row (↑/↓/Home/End/PageUp/PageDown, Enter activates, Space selects), sticky header when `height` is set, fixed-height windowing above `virtualizeThreshold` (default 200 rows) with `aria-rowcount`/`aria-rowindex`; the active row (the body's tab stop) stays mounted when scrolled out of the window, so focus and the Tab entry point survive scrolling. Interactive cells keep their own controls. |
| `Dialog`, `Drawer` | Portal, `role="dialog"`, `aria-modal`, labelled by the title, described by the description, focus trapped, Esc and the Close button close, focus returns to the opener, body scroll locked (one lock shared by nested overlays: the page's overflow is restored when the last one closes, in any order). `Drawer` slides from the end (or `side="start"`). |
| `Menu` | APG menu button: Enter/Space/↓ opens on the first item, ↑ on the last; arrows wrap; Home/End; type-ahead; Esc restores focus; Tab and outside click close. |
| `Tooltip` | Hover (delayed) and focus (immediate); `aria-describedby` while visible; Esc dismisses; stays while hovered (WCAG 1.4.13). Supplementary text only. |
| `ToastProvider`/`useToast` | Region "Notifications" with a polite live list; success/info auto-dismiss after 6s (paused while hovered or focused), errors persist until dismissed. Mounted inside the router (root route), so an `action` may be a `ButtonLink`. |
| `Banner` | Info/warning/error/success with icon and a visually hidden tone name; `live` adds `role="status"` (or `alert` for errors). |
| `EmptyState` | Heading, explanation, primary and secondary action; `headingLevel` 1 for whole-page states. |
| `Skeleton`, `SkeletonText` | `aria-hidden`; mark the loading container `aria-busy`. |
| `Badge` | Short text label; tone reinforces the text. |
| `Breadcrumbs` | `nav[aria-label=Breadcrumb]`, last item `aria-current="page"`. |
| `Pagination` | `nav[aria-label=Pagination]`, page size select, polite page indicator. |
| `SearchInput` | `type=search`; Esc clears, then blurs; Enter submits; clear button; optional shortcut hint. |
| `FilterBar`, `FilterChips` | Toolbar layout; chips with "Remove filter …" buttons and "Clear all filters". Chips use the 4px radius, not pills. |
| `KeyValueList` | `<dl>` grid; empty values render "—"; stacks below 480px. |
| `Code`, `Mono`, `CodeBlock` | Inline code, monospace identifiers, and a figure with title, copy button and a focusable, named scroll region. |
| `CopyButton` | Clipboard write with a polite "Copied to clipboard" / failure announcement. |
| `Timestamp` | Relative text in `<time datetime>`; absolute local time plus UTC in a tooltip and in visually hidden text. |
| `Split` | Two panes with a focusable `role="separator"` (←/→ by 5%, Home/End); stacks below 768px. |
| `PageHeader` | The page `h1`, description and actions. |

## Status

Health, sync, operation phase and connection state each have their own glyph family so that
the four can sit side by side in one row without being confused, even in grayscale or for
users who cannot distinguish the colors. All glyphs are `aria-hidden`; the label is always
present (visually hidden with `hideLabel`, then also exposed as a `title`). Unknown values
render with the `Unknown` (or `Pending`) glyph and their raw text.

| Family | Component | Visual grammar | Placement |
|--------|-----------|----------------|-----------|
| Health | `HealthStatus` | Solid silhouettes, every state a different outline shape | Bare glyph before the label |
| Sync | `SyncStatus` | Comparison strokes between "desired" and "live": `=`, `≠`, dashed `=` | Glyph inside a 16px bordered square, then the label |
| Operation | `OperationPhaseStatus` | Bare process strokes (no container shape of their own) | Glyph and label inside a bordered tag (12px type) |
| Connection | `ConnectionStatus` | Two endpoints and a link | Bare glyph before the label |

| State | Shape | Color token |
|-------|-------|-------------|
| Healthy | solid circle with a check | `--status-healthy` (success) |
| Progressing | ring with the left half filled | `--status-progressing` (info) |
| Degraded | solid triangle with "!" | `--status-degraded` (danger) |
| Suspended | solid square with pause bars | `--status-suspended` |
| Missing | dashed ring | `--status-missing` (warning) |
| Unknown | solid diamond with "?" | `--status-unknown` |
| Synced | `=` in a box | `--status-synced` (success) |
| OutOfSync ("Out of sync") | `≠` in a box | `--status-outofsync` (warning) |
| Unknown (sync) | dashed `=` in a box | `--status-unknown` |
| Pending | hourglass | unknown |
| Running | play triangle (outline) | info |
| Terminating | stop square (outline) | warning |
| Succeeded | check mark | success |
| Failed | cross | danger |
| Error | exclamation mark | danger |
| Cancelled | circle with a slash | unknown |
| Rejected | return arrow | warning |
| Successful | two dots joined by a line | success |
| Failed (connection) | two dots, broken line with a slash | danger |
| Unknown (connection) | two hollow dots, dotted line | unknown |

`src/design/status/Status.test.tsx` asserts that every state in each family has a different
glyph, that no glyph is shared between the health, sync and operation families, that labels
are present, and the box/tag placement.

## Accessibility baseline

Every component meets these, and pages composed from them inherit it:

- Visible focus ring (`--color-focus`, 2px, offset 2px) on every interactive element;
  no focus styles are removed without a replacement.
- Keyboard operation for everything a pointer can do; arrow-key patterns follow the WAI-ARIA
  Authoring Practices (tabs, menu button, dialog, window splitter).
- Names: icon-only controls have `aria-label`; tables have captions; landmarks are labelled
  (`Main`, `Breadcrumb`, `Pagination`, `Notifications`, `Announcement`).
- Live regions: toasts (polite), copy confirmations (polite), live banners
  (`status`/`alert`).
- Targets are at least 24×24px (controls 28–32px).
- Text and essential graphics meet the contrast table above in both themes.

## Adding a component

1. Put it in `src/design/components/<Name>.tsx` with a `<Name>.module.css` (or extend an
   existing family file) and export it from `src/design/index.ts`.
2. Use tokens only (no literal colors, radii above `--radius-md`, gradients or blur).
3. Write `<Name>.test.tsx` covering roles, names, keyboard interaction and states, using
   Testing Library role queries and `user-event`.
4. Add it to the gallery (`src/pages/dev/DesignGalleryPage.tsx`) and to the table above.
