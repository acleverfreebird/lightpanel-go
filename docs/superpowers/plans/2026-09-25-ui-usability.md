# UI usability implementation plan

Goal: Make the existing six-module server workspace fully usable with clear feedback and responsive presentation.

Design: Keep the lightweight emerald/slate workspace and native HTML/JS architecture. Prioritize connecting the existing UI over adding new infrastructure. A purely cosmetic refresh would leave broken workflows; a framework rewrite would add unnecessary dependencies. The selected approach improves presentation and completes the existing interactions.

User authorization: The user explicitly delegated UI design and implementation decisions. Implement directly in the current workspace (no Git repository present).

- [x] Connect navigation, refresh state, errors and read-only controls in static/app.js using static/ui.js.
- [x] Build real metric cards and a bounded, accessible live resource chart in static/overview.js.
- [x] Complete process, service, file, log and firewall workflows in focused modules; retain all existing API contracts and confirmation boundaries.
- [x] Improve typography, contrast, responsive navigation, table scrolling and visible busy states in static/app.css.
- [x] Verify module initialization and all six screens in a browser; run Linux Go tests, vet and build; record results in docs/UI-VALIDATION.md.

Validation cases: initial load without JavaScript exceptions; refresh and pause; hash navigation and back; service search/status filtering/pagination; process search/reset; file breadcrumbs and upload; dialog cancel/confirm; log filter/wrap; engine-specific firewall options; read-only disables mutations; narrow viewport has no page-wide overflow; API errors remain visible and refresh recovers.
