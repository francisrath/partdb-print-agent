// Served from 'self' so the page CSP can forbid inline scripts.
document.querySelectorAll('[data-status]').forEach(async (td) => {
  try {
    const r = await fetch('/printers/' + encodeURIComponent(td.dataset.status) + '/status');
    const s = await r.json();
    td.textContent = s.text;
    td.className = s.ok ? '' : 'err';
  } catch (e) {
    td.textContent = 'unknown';
  }
});

document.querySelectorAll('form[data-confirm]').forEach((f) => {
  f.addEventListener('submit', (e) => { if (!confirm(f.dataset.confirm)) e.preventDefault(); });
});

const ms = Number(document.body.dataset.autoclose || 0);
if (ms) setTimeout(() => window.close(), ms);
