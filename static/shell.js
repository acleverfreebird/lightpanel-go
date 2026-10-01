import { $, el } from './ui.js';
import { icon } from './icons.js';
import { matchNavigation } from './navigation.js';

// Shell interactions only; module navigation and API state stay in app.js.
export function setupShell(pages, go) {
  const sidebar = $('.sidebar'), toggle = $('#menu-toggle'), scrim = $('#nav-scrim');
  const shell = $('.shell'), mobile = matchMedia('(max-width: 760px)');
  const closeNav = (restore = false) => {
    const wasOpen = document.body.classList.contains('nav-open');
    document.body.classList.remove('nav-open');
    toggle.setAttribute('aria-expanded', 'false');
    sidebar.inert = mobile.matches;
    shell.inert = false;
    scrim.hidden = true;
    if (restore && wasOpen) toggle.focus();
  };
  toggle.addEventListener('click', () => {
    document.body.classList.add('nav-open');
    toggle.setAttribute('aria-expanded', 'true');
    sidebar.inert = false;
    shell.inert = true;
    scrim.hidden = false;
    $('#nav-close').focus();
  });
  $('#nav-close').addEventListener('click', () => closeNav(true));
  scrim.addEventListener('click', () => closeNav(true));
  sidebar.addEventListener('click', event => {
    if (event.target.closest('[data-tab], .brand, #task-center-open')) closeNav(true);
  }, { capture: true }); // Restore the trigger before a target handler opens a modal.
  mobile.addEventListener('change', () => closeNav(true));
  window.addEventListener('hashchange', () => closeNav());
  closeNav();

  const keywords = { overview: 'dashboard cpu memory 概览', sites: 'nginx apache docker ssl 域名 证书',
    databases: 'mysql mariadb postgresql redis sql', apps: 'install 安装 软件', files: 'upload download 上传 下载',
    terminal: 'shell bash ssh', processes: 'pid cpu', services: 'systemd restart', firewall: 'port ufw firewalld 安全', logs: 'journalctl audit 审计' };
  const entries = Object.entries(pages).map(([id, [title, description]]) => ({ id, title, description, keywords: keywords[id] }));
  const dialog = $('#command-dialog'), input = $('#command-input'), results = $('#command-results');
  let matched = [], selected = 0;
  function highlight() {
    [...results.children].forEach((button, i) => {
      button.classList.toggle('selected', i === selected);
      button.setAttribute('aria-selected', String(i === selected));
    });
    if (matched.length) {
      input.setAttribute('aria-activedescendant', `command-${matched[selected].id}`);
      results.children[selected].scrollIntoView({ block: 'nearest' });
    } else input.removeAttribute('aria-activedescendant');
  }
  function choose(index) {
    const entry = matched[index];
    if (!entry) return;
    dialog.close();
    go(entry.id);
    $('#workspace').focus({ preventScroll: true });
  }
  function render() {
    matched = matchNavigation(entries, input.value);
    selected = 0;
    results.replaceChildren(...matched.map((entry, i) => {
      const button = el('button', undefined, 'command-result');
      button.type = 'button';
      button.id = `command-${entry.id}`;
      button.tabIndex = -1;
      button.setAttribute('role', 'option');
      const symbol = el('span', undefined, 'command-icon');
      symbol.append(icon(entry.id === 'overview' ? 'home' : entry.id));
      const copy = el('span', undefined, 'command-copy');
      copy.append(el('strong', entry.title), el('small', entry.description));
      button.append(symbol, copy, el('span', '↵', 'command-enter'));
      button.addEventListener('click', () => choose(i));
      return button;
    }));
    $('#command-empty').hidden = matched.length > 0;
    $('#command-count').textContent = `${matched.length} 个功能`;
    highlight();
  }
  function openSearch() {
    if (document.querySelector('dialog[open]')) return;
    // Same order as the sidebar capture handler: return focus to the toggle
    // before showModal(), or the dialog would later restore it into the inert sidebar.
    closeNav(true);
    input.value = '';
    dialog.showModal();
    render();
    input.focus();
  }
  $('#command-open').addEventListener('click', openSearch);
  $('#command-close').addEventListener('click', () => dialog.close());
  input.addEventListener('input', render);
  input.addEventListener('keydown', event => {
    if (event.isComposing) return;
    if (event.key === 'ArrowDown' || event.key === 'ArrowUp') {
      event.preventDefault();
      selected = matched.length ? (selected + (event.key === 'ArrowDown' ? 1 : -1) + matched.length) % matched.length : 0;
      highlight();
    } else if (event.key === 'Enter') { event.preventDefault(); choose(selected); }
  });
  document.addEventListener('keydown', event => {
    if (event.key === 'Escape' && document.body.classList.contains('nav-open')) { event.preventDefault(); closeNav(true); }
    if ((event.ctrlKey || event.metaKey) && event.key.toLowerCase() === 'k' && !event.isComposing) {
      if (document.querySelector('dialog[open]') || event.target.closest('#terminal-container')) return;
      event.preventDefault(); openSearch();
    }
    if (event.key === 'Tab' && document.body.classList.contains('nav-open')) {
      const items = [...sidebar.querySelectorAll('a, button')].filter(node => !node.disabled && node.getClientRects().length);
      const first = items[0], last = items.at(-1);
      if (event.shiftKey && document.activeElement === first) { event.preventDefault(); last.focus(); }
      else if (!event.shiftKey && document.activeElement === last) { event.preventDefault(); first.focus(); }
    }
  });
}
