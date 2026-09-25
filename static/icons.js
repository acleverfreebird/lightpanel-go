// Small, local icon vocabulary. No fonts, network requests or HTML injection.
const paths = {
  overview: 'M3 3h7v7H3z M14 3h7v7h-7z M3 14h7v7H3z M14 14h7v7h-7z',
  processes: 'M3 12h4l3-8 4 16 3-8h4',
  services: 'M4 4h16v6H4z M4 14h16v6H4z M7 7h.01 M7 17h.01 M15 7h2 M15 17h2',
  apps: 'M12 3l9 5-9 5-9-5z M3 8v9l9 5 9-5V8 M12 13v9',
  sites: 'M3 5h18v14H3z M3 9h18 M6 7h.01 M9 7h.01 M7 13h4 M7 16h7',
  databases: 'M4 6c0-4 16-4 16 0s-16 4-16 0 M4 6v12c0 4 16 4 16 0V6 M4 12c0 4 16 4 16 0',
  files: 'M3 7h7l2 2h9v11H3z M3 7V4h6l3 3h8v2',
  logs: 'M5 3h10l4 4v14H5z M14 3v5h5 M8 12h8 M8 16h6',
  firewall: 'M12 3l8 3v6c0 5-8 9-8 9S4 17 4 12V6z M8 12l3 3 5-6',
  refresh: 'M20 8a8 8 0 0 0-14-3L3 8 M3 3v5h5 M4 16a8 8 0 0 0 14 3l3-3 M21 21v-5h-5',
  arrow: 'M6 18L18 6 M6 6h12v12',
  search: 'M16 10a6 6 0 1 1-12 0 6 6 0 0 1 12 0 M15 15l6 6',
  cpu: 'M6 6h12v12H6z M9 9h6v6H9z M9 3v3 M15 3v3 M9 18v3 M15 18v3 M3 9h3 M3 15h3 M18 9h3 M18 15h3',
  memory: 'M3 6h18v11H3z M7 10v3 M12 10v3 M17 10v3 M6 17v3 M10 17v3 M14 17v3 M18 17v3',
  disk: 'M6 4h12l3 10v6H3v-6z M3 14h18 M16 17h2',
  network: 'M8 3v17 M3 8l5-5 5 5 M16 21V4 M11 16l5 5 5-5',
};

export function icon(name) {
  const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
  svg.setAttribute('viewBox', '0 0 24 24');
  svg.setAttribute('class', 'icon');
  svg.setAttribute('aria-hidden', 'true');
  svg.setAttribute('focusable', 'false');
  const path = document.createElementNS('http://www.w3.org/2000/svg', 'path');
  path.setAttribute('d', paths[name] || paths.overview);
  svg.append(path);
  return svg;
}

export function hydrateIcons() {
  document.querySelectorAll('[data-icon]').forEach(node => node.replaceChildren(icon(node.dataset.icon)));
}
