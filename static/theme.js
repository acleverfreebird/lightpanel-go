import { $, api, el, guard, message, mutate } from './ui.js';
import { confirmAction } from './ui.js';
import { icon } from './icons.js';
import { size } from './format.js';

// 外观模块：渲染主题卡片、上传/激活/删除主题，并把切换即时应用到当前
// 页面（body 的 data-theme + 替换 <link>），无需刷新。其他模块监听
// document 上的 themechange 事件重绘跟随主题的内容（图表、终端）。

// 把激活主题热应用到当前文档。activate 接口返回 {id,url,scheme}。
function applyTheme({ id, url, scheme }) {
  document.body.dataset.theme = id;
  const schemeStyle = $('#scheme-style');
  if (schemeStyle) schemeStyle.textContent = `:root{color-scheme:${scheme || 'light'}}`;
  let link = $('#theme-style');
  if (!url) {
    link?.remove();
  } else {
    if (!link) {
      link = document.createElement('link');
      link.id = 'theme-style';
      link.rel = 'stylesheet';
      document.head.append(link);
    }
    if (link.getAttribute('href') !== url) link.setAttribute('href', url);
  }
  document.dispatchEvent(new CustomEvent('themechange'));
}

function swatch(theme) {
  const swatch = el('span', undefined, 'theme-swatch');
  swatch.setAttribute('aria-hidden', 'true');
  for (const key of ['bg', 'surface', 'accent', 'text']) {
    const chip = el('i');
    const color = theme.preview?.[key];
    if (color) chip.style.background = color;
    swatch.append(chip);
  }
  return swatch;
}

export function setupAppearance() {
  const list = $('#theme-list'), uploadInput = $('#theme-upload');
  let themes = new Map(), activeId = 'light';

  function render(list_data, active) {
    activeId = active;
    themes = new Map((list_data || []).map(theme => [theme.id, theme]));
    $('#theme-count').textContent = String(list_data?.length ?? 0);
    if (!list_data?.length) {
      const empty = el('div', undefined, 'empty-state');
      empty.append(el('strong', '暂无主题'), el('p', '上传一个 CSS 主题文件试试。'));
      list.replaceChildren(empty);
      return;
    }
    list.replaceChildren(...list_data.map(theme => {
      const card = el('button', undefined, 'theme-card');
      card.type = 'button';
      card.dataset.themeId = theme.id;
      card.setAttribute('aria-pressed', String(theme.id === activeId));
      if (theme.id === activeId) card.classList.add('active');
      card.append(swatch(theme));
      const head = el('div', undefined, 'theme-card-head');
      head.append(el('h3', theme.name || theme.id));
      head.append(el('span', theme.builtin ? '内置' : '自定义', 'badge'));
      card.append(head);
      if (theme.id === activeId) {
        const mark = el('span', undefined, 'theme-active-mark');
        mark.append(icon('check'), '使用中');
        card.append(mark);
      } else {
        card.append(el('span', '点击应用', 'muted'));
      }
      const meta = el('div', undefined, 'theme-meta');
      meta.append(el('span', theme.builtin ? '随面板分发' : `${size(theme.size)} · ${theme.updated ? new Date(theme.updated).toLocaleDateString() : ''}`));
      if (!theme.builtin) {
        const remove = el('button', '删除', 'danger-text');
        remove.type = 'button';
        remove.title = readOnly ? '当前账号只有查看权限' : `删除主题 ${theme.name || theme.id}`;
        remove.disabled = readOnly;
        remove.addEventListener('click', guard(async event => {
          event.stopPropagation();
          const yes = await confirmAction({ title: '删除主题', description: `主题「${theme.name || theme.id}」将被删除；如果它正在使用，界面会回到默认浅色。`, target: theme.id, confirm: '删除主题' });
          if (!yes) return;
          const result = await mutate('/api/theme/delete', { id: theme.id });
          if (result.active === 'light') {
            const light = themes.get('light');
            applyTheme({ id: 'light', url: light?.url || '/static/themes/light.css', scheme: light?.scheme || 'light' });
          }
          await refresh();
        }));
        meta.append(remove);
      }
      card.append(meta);
      return card;
    }));
  }

  async function refresh() {
    const data = await api('/api/theme');
    render(data.themes || [], data.active || 'light');
    return data;
  }

  async function activate(id) {
    if (id === activeId) return;
    const result = await mutate('/api/theme/activate', { id });
    applyTheme(result);
    activeId = result.id;
    document.querySelectorAll('.theme-card').forEach(card => {
      const on = card.dataset.themeId === result.id;
      card.classList.toggle('active', on);
      card.setAttribute('aria-pressed', String(on));
    });
    message(`已切换到主题「${result.name || result.id}」。`);
  }

  list.addEventListener('click', event => {
    const card = event.target.closest('.theme-card');
    if (!card || event.target.closest('.danger-text')) return;
    activate(card.dataset.themeId).catch(error => message(error.message, true));
  });

  if (uploadInput) {
    uploadInput.addEventListener('change', guard(async () => {
      const file = uploadInput.files?.[0];
      if (!file) return;
      if (file.size > 256 * 1024) throw new Error('主题文件超过 256 KiB 上限。');
      const base = file.name.replace(/\.css$/i, '');
      const slug = base.toLowerCase().replace(/[^a-z0-9_-]+/g, '-').replace(/^[-_]+|[-_]+$/g, '').slice(0, 32) || 'theme';
      const result = await mutate(`/api/theme/upload?name=${encodeURIComponent(slug)}&label=${encodeURIComponent(base.slice(0, 48))}`, file, true);
      uploadInput.value = '';
      await refresh();
      message(`主题「${result.theme.name}」已上传，点击卡片即可应用。`);
    }));
  }

  return { refresh, activate };
}
