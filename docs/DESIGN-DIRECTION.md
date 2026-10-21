# Console redesign direction (UX/WebUI-overhaul)

This is the design contract for the WebUI overhaul. It works from `docs/UI-INVENTORY.md`
(every page, route, and admin/user gate must survive the redesign unchanged in behavior).

## Principles

1. Professional and calm: fewer gradients, tighter type, one accent. The existing warm
   palette (cream surfaces, ink navy, terracotta accent) stays because it is the product's
   identity; what changes is discipline and density.
2. Easy navigation: five tasks (Mailboxes, Caller access, Voice & alerts, Security,
   Administration) as a persistent navigation, never a mystery. The operational status
   panel stays visible or one tap away on every viewport.
3. Non-technical first: plain labels, one primary action per card, help text on the fields
   that need it, destructive actions clearly marked and confirmed.
4. No functionality loss: every control, API call, aria-live announcement, and admin/user
   difference in the inventory keeps working. The redesign changes markup and styles, not
   behavior. `window.isAdmin` gating and `.alert-control` hiding stay exactly as they are.

## Design tokens (extends the existing `:root` variables; names kept, values refined)

- Color: keep `--bg` cream canvas, `--surface`/`--surface-soft` cards, `--ink` text,
  `--muted` secondary text, `--line`/`--line-strong` borders, `--accent` terracotta with
  `--accent-dark` hover, `--accent-soft` selected/hover washes, `--navy` headings,
  `--success`/`--danger` states. Add: `--info` (blue) for neutral notices, `--focus` ring
  color (accent), `--overlay` for modal scrims.
- Type scale: Inter for all UI text; Georgia serif reserved for the brand lockup and the
  auth story. Scale: 13px small / 14px body-small / 15px body / 17px card titles / 20px
  section titles / 28px page titles / 40px brand display (down from the 5.2rem hero).
- Spacing: 4px base grid; card padding 20-24px desktop, 16px mobile; section gaps 24-32px.
- Radius: `--radius` 18px for cards, 10px for inputs/buttons/badges, 999px pills.
- Shadows: single soft elevation (`--shadow`) for cards, a stronger one for modals and
  drawers. No stacked drop shadows.
- Focus: 2px accent ring with 2px offset on every interactive element (keyboard parity).

## Layout shell

- Desktop (>=1024px): fixed left sidebar (240px) with brand, the five task links, the
  admin-only Administration link (rendered only when `window.isAdmin`), and the
  operational status block pinned at the bottom of the sidebar. Content column max
  1200px with generous padding.
- Tablet (640-1023px): sidebar collapses to a slim icon rail or top bar; content full
  width with 24px gutters.
- Mobile (<640px): top bar with brand + menu button opening a slide-in drawer; bottom
  safe-area padding; all tap targets >=44px; content single-column; wide tables turn into
  stacked key/value cards.
- The operational status panel becomes a compact card in the sidebar (desktop) and a
  collapsible strip under the top bar (mobile). Its aria-live announcements keep working.

## Components (new classes, old classes kept working during migration)

- Cards: `.card` = surface + border + `--radius` + soft shadow; card header row with
  eyebrow label, title, and one primary action right-aligned.
- Buttons: `.primary` (accent fill), `.secondary` (surface + border), `.ghost` (text),
  `.danger` (red outline that fills on confirm). Consistent 10px radius, 44px min height.
- Inputs: 44px min height, 10px radius, `--line-strong` border, accent focus ring;
  labels above fields, `<small>` hints below, error text in `--danger`.
- Badges/pills: status badges (syncing, error, paused) with `--success`/`--danger`/
  `--info` soft backgrounds; count pills in nav.
- Tables: header row in `--surface-soft`, zebra-free rows separated by `--line`, row
  actions right-aligned; mobile stacked-card variant per section.
- Modals and drawers: `--overlay` scrim, focus-trapped dialogs, Escape to close,
  `aria-modal` kept from current markup.
- Toasts/announcements: the existing aria-live region styled as a floating toast stack.

## Auth states redesign (the options-based login)

`#auth` keeps its six states (loading, first-run setup, login, recovery tools, recovery
request, recovery confirm) and its `data-state` switching, but the login panel becomes a
menu of options instead of one form with three credential fields always visible:

1. Primary option: "Sign in with username and password" (the common case, one tap).
2. Option rows revealed on demand: "Use an authenticator code", "Use a backup code",
   each opening only its own field; picking one never hides the others' entry points.
3. A quiet "Recovery tools" link below the options opens the existing recovery states.
4. First-run setup stays its own full state (unchanged flow), visually matching the new
   card system.
5. The auth story column (eyebrow, headline, trust list) keeps its role on desktop and
   stacks above the panel on mobile.

## Migration plan

1. Land the token/base layer and layout shell (sidebar, top bar, drawer) first; keep every
   existing class name working so the 14 ES modules render untouched.
2. Re-skin section by section (mailboxes incl. folder editor/preview, caller-access,
   voice-alerts, security, administration), checking `docs/UI-INVENTORY.md` per section.
3. Rebuild the auth states last (options menu), reusing the same form endpoints and
   field names.
4. Verify: `go build ./...`, `go test ./internal/web/...`, browser tests in
   `/tests/browser` with a local chromium; screenshots of every page at 390px, 820px, and
   1440px widths for user review before any merge.

## 12ui usage

The 12ui design tool (draft -> branch -> convert) is the preferred generator for the
visual candidates, but it is currently rate-limited (429 "Too many active conversions"
since Oct 21 ~14:25 EDT, fresh requests included). The hand-built path above proceeds
without it; if 12ui unblocks, its candidates are used to polish the card and auth
directions via `12ui convert` (which needs only the already-granted `design.convert`
scope).
