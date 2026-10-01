# LightPanel UI implementation plan

**Goal:** Deliver a cohesive, modern administration workspace across all existing modules.

**Architecture:** Retain Go templates, API contracts, business modules and CSP. Rebuild shared visual tokens and layout in app.css; move feature-specific styles to components.css. Add a small shell module for navigation search and mobile drawer. Keep new interaction logic separate from API state.

**Tech stack:** Native CSS, JavaScript ES modules, SVG and Go html/template.

1. Define search matching behavior with node:test, covering Chinese, English, multiple terms and no results; run red before implementing static/navigation.js.
2. Rebuild templates/index.html shell and overview, preserving every existing module ID and form contract. Add accessible search dialog and mobile navigation controls. Wire static/shell.js from app.js.
3. Replace global styling and extract feature styles into static/components.css; restyle tables, dialogs, tasks, forms, terminal and responsive layouts. Update overview chart colors to the shared palette.
4. Rebuild templates/login.html and static/login.css with the same identity. Retain the login POST contract and autocomplete.
5. Run node --test scripts/*.test.mjs, syntax checks and Linux cross-compilation. Inspect desktop/mobile browser views, navigation, search, forms and task dialog; correct findings and record verification limits.

No backend behavior changes, new dependencies or production mutations are required. Keep the implementation in the user's current workspace for review.
