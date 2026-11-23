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

// Update now: poll until the restarted agent reports another version, or the update has stopped (failed).
// Unreachable for 2 minutes in a row (a download may take longer, but then the agent answers): it
// didn't come back, so say how to start it.
document.querySelectorAll('[data-updating]').forEach((el) => {
  let downSince = 0;
  const poll = async () => {
    try {
      const info = await (await fetch('/v1/info', { cache: 'no-store' })).json();
      if (info.version !== el.dataset.updating || !info.updating) { location.replace('/?manual'); return; }
      downSince = 0;
    } catch (e) { // restarting
      downSince = downSince || Date.now();
      if (Date.now() - downSince > 120000) {
        el.textContent = "pdb-print didn't come back after the update: sign out of Windows and in again " +
          '(the service: Restart-Service pdb-print in an administrator PowerShell), then reload this page.';
        el.classList.add('err');
        return;
      }
    }
    setTimeout(poll, 2000);
  };
  setTimeout(poll, 2000);
});

// Copy buttons next to commands: the clipboard API needs a secure context (127.0.0.1 is one, a LAN
// address over http isn't); without it, select the command so Ctrl+C copies it.
document.querySelectorAll('[data-copy]').forEach((b) => {
  b.addEventListener('click', async () => {
    try {
      await navigator.clipboard.writeText(b.dataset.copy);
      b.textContent = 'Copied';
      setTimeout(() => { b.textContent = 'Copy'; }, 2000);
    } catch (e) {
      const code = b.previousElementSibling;
      const r = document.createRange();
      r.selectNodeContents(code);
      const sel = window.getSelection();
      sel.removeAllRanges();
      sel.addRange(r);
      b.textContent = 'Press Ctrl+C';
    }
  });
});
