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

// Printer form: offer only the media the selected model takes.
const modelSel = document.querySelector('select[name="model"]');
const mediaSel = document.querySelector('select[name="media"]');
if (modelSel && mediaSel) {
  const filter = () => {
    for (const o of mediaSel.options) {
      if (!o.dataset.models) continue; // "as reported by the printer"
      o.hidden = !o.dataset.models.split(' ').includes(modelSel.value);
      if (o.hidden && o.selected) mediaSel.value = 'auto';
    }
  };
  modelSel.addEventListener('change', filter);
  filter();
}

// Setup wizard: open Part-DB's "new API token" page for the address typed above.
document.querySelectorAll('[data-token-page]').forEach((b) => {
  b.addEventListener('click', () => {
    let base = (b.form.elements.base_url.value || '').trim().replace(/\/+$/, '');
    if (!base) { b.form.elements.base_url.focus(); return; }
    if (!base.includes('://')) base = 'https://' + base;
    window.open(base + b.dataset.tokenPage, '_blank', 'noopener');
  });
});

// Steps that search for printers take up to 30 s: show it on the button that started one.
const busy = (el) => { el.textContent = el.dataset.busy; el.classList.add('busy'); el.setAttribute('aria-busy', 'true'); };
document.querySelectorAll('a[data-busy]').forEach((a) => a.addEventListener('click', () => busy(a)));
document.addEventListener('submit', (e) => { if (!e.defaultPrevented && e.submitter && e.submitter.dataset.busy) busy(e.submitter); });
