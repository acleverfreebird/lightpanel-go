# LightPanel interface redesign

The user delegates visual direction and implementation. Build a warm, editorial
server workspace: ivory canvas, white navigation, graphite host summary, burnt
orange actions, teal/violet/ochre resource accents. Avoid external assets and fonts.

## Scope

Rebuild the shared shell, overview, all nine module surfaces, login, forms,
tables, dialogs and responsive layouts. Preserve API contracts, form names,
element IDs, hash navigation, read-only enforcement and mutation confirmations.
Keep native JavaScript and Go embedded static files, with no runtime dependencies.

## Composition

- Group navigation into overview, deployment and system operations, with matching
  inline SVG icons and a persistent current-host card.
- Use a compact breadcrumb bar, generous page title, and contextual English kicker.
- Overview: dark host banner, four large metric cards with accessible gauges,
  CPU/memory chart and host details, shortcuts, diagnostics and version controls.
- Management pages: strong table headers, restrained borders, explicit actions,
  spacious forms, clear empty/loading/error states and readable terminal output.
- Login: dark typographic brand panel with decorative server illustration and a
  focused light login form. Preserve native password manager support.
- At tablet widths, compact the navigation; at phone widths use a horizontally
  scrollable module strip, stack content, and contain wide tables in their panels.

## Validation

Run existing JavaScript tests, Linux-targeted Go build/vet and Go tests under WSL
where available. Inspect real loopback preview in browser at desktop and mobile
sizes; check every module, login, navigation, search, dialog cancellation, errors
and read-only controls. Never mutate real services or firewall rules for UI QA.
