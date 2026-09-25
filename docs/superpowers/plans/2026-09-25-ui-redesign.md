# UI redesign implementation plan

**Goal:** Deliver a complete visual redesign of LightPanel's login and nine modules.
**Architecture:** Keep existing API modules; replace the shared stylesheet and shell,
with a separate login stylesheet and small shared SVG icon module.
**Tech stack:** Go templates, native CSS and JavaScript, local SVG.

## Constraints

No external fonts, CDN, framework, fake metrics, changed backend contracts or new
production dependencies. Preserve IDs, form fields, read-only and confirmation flows.

## Implementation

- [ ] Rebuild `static/app.css` with tokens, shell, responsive navigation, reusable
  cards/tables/forms/dialogs and metric styles. Split login into `static/login.css`.
- [ ] Recompose `templates/index.html` and `templates/login.html`; add grouped
  navigation, host banner and coherent icons without removing functional elements.
- [ ] Add `static/icons.js`; update `static/app.js` to hydrate icon placeholders and
  page kickers. Restyle `static/overview.js` gauges and chart using the new palette.
- [ ] Run `node --test scripts/*.test.mjs`, Linux Go build and vet, and available
  Linux tests. Use the existing disposable preview for real browser inspection.
- [ ] Inspect desktop/mobile login and all modules, filtering, confirmation cancel,
  read-only controls, console errors and page overflow. Record results and limits.
