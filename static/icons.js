// Small, local icon vocabulary. No fonts, network requests or HTML injection.
const paths = {
  menu: 'M4 6h16 M4 12h16 M4 18h16',
  close: 'M6 6l12 12 M6 18L18 6',
  home: 'M4 11l8-7 8 7 M6 10v10h12V10 M10 20v-6h4v6',
  processes: 'M3 12h4l3-8 4 16 3-8h4',
  services: 'M4 4h16v6H4z M4 14h16v6H4z M7 7h.01 M7 17h.01 M15 7h2 M15 17h2',
  apps: 'M12 3l9 5-9 5-9-5z M3 8v9l9 5 9-5V8 M12 13v9',
  sites: 'M3 5h18v14H3z M3 9h18 M6 7h.01 M9 7h.01 M7 13h4 M7 16h7',
  databases: 'M4 6c0-4 16-4 16 0s-16 4-16 0 M4 6v12c0 4 16 4 16 0V6 M4 12c0 4 16 4 16 0',
  files: 'M3 7h7l2 2h9v11H3z M3 7V4h6l3 3h8v2',
  logs: 'M5 3h10l4 4v14H5z M14 3v5h5 M8 12h8 M8 16h6',
  terminal: 'M3 5h18v14H3z M7 9l3 3-3 3 M13 15h5',
  firewall: 'M12 3l8 3v6c0 5-8 9-8 9S4 17 4 12V6z M8 12l3 3 5-6',
  refresh: 'M20 8a8 8 0 0 0-14-3L3 8 M3 3v5h5 M4 16a8 8 0 0 0 14 3l3-3 M21 21v-5h-5',
  arrow: 'M6 18L18 6 M6 6h12v12',
  search: 'M16 10a6 6 0 1 1-12 0 6 6 0 0 1 12 0 M15 15l6 6',
  cpu: 'M6 6h12v12H6z M9 9h6v6H9z M9 3v3 M15 3v3 M9 18v3 M15 18v3 M3 9h3 M3 15h3 M18 9h3 M18 15h3',
  memory: 'M3 6h18v11H3z M7 10v3 M12 10v3 M17 10v3 M6 17v3 M10 17v3 M14 17v3 M18 17v3',
  disk: 'M6 4h12l3 10v6H3v-6z M3 14h18 M16 17h2',
  network: 'M8 3v17 M3 8l5-5 5 5 M16 21V4 M11 16l5 5 5-5',
  tasks: 'M8 3h8v4H8z M5 5v16h14V5 M9 13l2 2 4-4',
  server: 'M4 4h16v6H4z M4 14h16v6H4z M7.5 7h.01 M7.5 17h.01 M16 7h1 M16 17h1',
  plus: 'M12 5v14 M5 12h14',
  trash: 'M4 7h16 M9 7V4h6v3 M6 7l1 14h10l1-14 M10 11v6 M14 11v6',
  copy: 'M9 9h11v11H9z M5 15V4h11',
  cut: 'M8 4l8 12 M16 4L8 16 M7 20a2.5 2.5 0 1 0 0-5 2.5 2.5 0 0 0 0 5 M17 20a2.5 2.5 0 1 0 0-5 2.5 2.5 0 0 0 0 5',
  paste: 'M9 4h6v3H9z M15 5h4v16H5V5h4 M8 12h8 M8 16h5',
  lock: 'M6 11h12v9H6z M8 11V7a4 4 0 0 1 8 0v4 M9.5 15.5h.01',
  appearance: 'M12 3a9 9 0 0 0 0 18c1.2 0 2-.9 2-2 0-.6-.2-.9-.6-1.4-.4-.5-.6-.9-.6-1.4 0-1.1.9-2 2-2h2.2A4.8 4.8 0 0 0 21 9.5C20.4 5.9 16.6 3 12 3z M7.5 11.5h.01 M10.5 7.5h.01 M15 7h.01',
  upload: 'M12 16V4 M7 9l5-5 5 5 M5 20h14',
  download: 'M12 4v12 M7 11l5 5 5-5 M5 20h14',
  check: 'M5 12l5 5 9-10',
};

export function icon(name) {
  const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
  svg.setAttribute('viewBox', '0 0 24 24');
  svg.setAttribute('class', 'icon');
  svg.setAttribute('aria-hidden', 'true');
  svg.setAttribute('focusable', 'false');
  const path = document.createElementNS('http://www.w3.org/2000/svg', 'path');
  path.setAttribute('d', paths[name] || paths.home);
  svg.append(path);
  return svg;
}

export function hydrateIcons() {
  document.querySelectorAll('[data-icon]').forEach(node => node.replaceChildren(icon(node.dataset.icon)));
}
