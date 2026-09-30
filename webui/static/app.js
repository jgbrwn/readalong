const $ = (selector) => document.querySelector(selector);
let shelfQuery = '';
let shelfSequence = 0;
let shelfSearchTimer = 0;
let pairResults = [];
let selectedPair = null;
let retranscriptionTarget = null;

async function api(path, options = {}) {
  const response = await fetch(path, options);
  if (!response.ok) {
    const detail = (await response.text()).trim();
    throw new Error(detail || `Request failed (${response.status})`);
  }
  if (response.status === 204) return null;
  return response.json();
}

function escapeHTML(value) {
  return String(value ?? '').replace(/[&<>'"]/g, (char) => ({
    '&': '&amp;', '<': '&lt;', '>': '&gt;', "'": '&#39;', '"': '&quot;',
  })[char]);
}

function formatDuration(ms) {
  if (!ms) return '';
  const total = Math.floor(ms / 1000);
  const hours = Math.floor(total / 3600);
  const minutes = Math.floor((total % 3600) / 60);
  return hours ? `${hours} hr ${minutes} min` : `${minutes} min`;
}

function progressLabel(book) {
  if (book.stage === 'retranscribing' && book.job_status === 'error') {
    return 'Fresh transcript failed · current version kept';
  }
  if (book.stage === 'retranscribing' && book.job_status === 'running') {
    return 'Fresh transcript in progress · current version ready';
  }
  if (book.stage === 'retranscribing' && book.job_status === 'queued') {
    return 'Fresh transcript queued · current version ready';
  }
  if (book.stage === 'aligning_retranscription') return 'Fresh transcript ready · aligning EPUB…';
  if (book.error) return book.error;
  if (book.stage === 'aligning') return book.status === 'ready' ? 'Ready · aligning ebook' : 'Aligning ebook text…';
  if (book.status === 'ready') {
    return book.stage === 'transcribing' ? 'Ready to read · finishing transcript' : 'Ready to read';
  }
  if (book.stage === 'acquiring') return 'Finding the audio…';
  if (book.stage === 'normalizing') return 'Preparing audio…';
  if (book.stage === 'rate_limited') return 'Groq limit reached · queued to resume';
  if (book.stage === 'transcribing') return 'Transcribing…';
  if (book.status === 'error') return 'Could not process this book';
  return 'Queued';
}

function coverColor(title) {
  let hash = 0;
  for (const char of title) hash = (hash * 31 + char.charCodeAt(0)) | 0;
  return Math.abs(hash) % 5;
}

async function loadBooks() {
  const root = $('#books');
  const request = ++shelfSequence;
  try {
    const params = new URLSearchParams();
    if (shelfQuery) params.set('q', shelfQuery);
    const query = params.toString();
    const books = (await api(`/api/books${query ? `?${query}` : ''}`)) || [];
    if (request !== shelfSequence) return;
    document.body.classList.toggle('has-books', books.length > 0 || Boolean(shelfQuery));
    $('#heroAdd').textContent = books.length || shelfQuery ? 'Add another book ↗' : 'Add your first book ↗';
    $('#bookCount').textContent = shelfQuery
      ? `${books.length} ${books.length === 1 ? 'match' : 'matches'}`
      : books.length ? `${books.length} ${books.length === 1 ? 'book' : 'books'}` : '';
    if (!books.length) {
      root.innerHTML = shelfQuery
        ? `<div class="empty-shelf"><h3>No matches on this shelf.</h3>
            <p>Try another title or author, or clear the search.</p>
            <button class="button button-quiet" id="clearShelfSearch">Clear search</button></div>`
        : `<div class="empty-shelf">
            <div class="empty-mark" aria-hidden="true">↗</div>
            <h3>Your next chapter starts here.</h3>
            <p>Add audio on its own, pair it with an EPUB, or find a free paired book.</p>
            <button class="button button-dark" id="emptyAdd">Add a book</button>
          </div>`;
      const emptyAdd = $('#emptyAdd');
      if (emptyAdd) emptyAdd.onclick = openImport;
      const clear = $('#clearShelfSearch');
      if (clear) clear.onclick = () => { shelfQuery = ''; $('#shelfSearch').value = ''; loadBooks(); };
      return;
    }
    root.innerHTML = books.map((book) => {
      const title = book.title || 'Untitled';
      const percentage = Math.max(0, Math.min(100, Math.round((book.progress || 0) * 100)));
      const subtitle = [book.author, formatDuration(book.duration_ms)].filter(Boolean).join(' · ');
      const jobActive = book.job_status === 'queued' || book.job_status === 'running';
      const retranscribeLabel = book.stage === 'rate_limited'
        ? 'Waiting for Groq…'
        : book.stage === 'aligning_retranscription'
          ? 'Aligning fresh transcript…'
        : book.stage === 'retranscribing' && book.job_status === 'queued'
          ? 'Fresh transcript queued…'
          : book.stage === 'retranscribing' && book.job_status === 'running'
            ? 'Re-transcribing…'
            : jobActive ? 'Transcription in progress…' : 'Re-transcribe';
      return `<article class="book-card">
        <a class="book-link" href="/reader/${encodeURIComponent(book.id)}" aria-label="Open ${escapeHTML(title)}">
          <div class="book-cover cover-${coverColor(title)}">
            <span class="cover-kicker">${book.source_kind === 'youtube' ? 'AUDIOBOOK' : 'READALONG'}</span>
            <strong>${escapeHTML(title)}</strong>
            <span class="cover-author">${escapeHTML(book.author || '')}</span>
          </div>
          <div class="book-info">
            <div class="book-state"><span class="state-dot state-${escapeHTML(book.status)}"></span>${escapeHTML(progressLabel(book))}</div>
            <h3>${escapeHTML(title)}</h3>
            <p>${escapeHTML([subtitle, book.mode === 'aligned' ? 'Audio + EPUB' : 'Transcript'].filter(Boolean).join(' · '))}</p>
            <div class="progress-track"><span style="width:${percentage}%"></span></div>
          </div>
        </a>
        ${book.status === 'ready' ? `<div class="book-card-actions">
          <button class="retranscribe-book" type="button" data-retranscribe-id="${escapeHTML(book.id)}"
            data-retranscribe-title="${escapeHTML(title)}" ${jobActive ? 'disabled' : ''}>
            ${escapeHTML(retranscribeLabel)}
          </button>
        </div>` : ''}
        <button class="delete-book" type="button" data-delete-id="${escapeHTML(book.id)}" aria-label="Remove ${escapeHTML(title)}" title="Remove book">×</button>
      </article>`;
    }).join('');
  } catch (error) {
    if (request !== shelfSequence) return;
    root.innerHTML = `<p class="empty">${escapeHTML(error.message || 'Your bookshelf could not be loaded.')}</p>`;
  }
}

async function boot() {
  try {
    const user = await api('/api/me');
    $('#who').textContent = user.email || '';
    $('#who').title = user.email || '';
    $('#who').setAttribute('aria-label', user.email ? `Signed in as ${user.email}` : 'Signed in');
    if (user.role === 'admin') $('#adminToggle').hidden = false;
  } catch {
    $('#who').textContent = 'Authentication required';
    $('#who').title = 'Authentication required';
  }
  await loadBooks();
}

function openImport() {
  $('#importError').textContent = '';
  $('#addDialog').showModal();
}

$('#add').addEventListener('click', openImport);
$('#heroAdd').addEventListener('click', openImport);
$('#closeImport').addEventListener('click', () => $('#addDialog').close());
$('#discoverPairs').addEventListener('click', openDiscover);
$('#heroDiscover').addEventListener('click', openDiscover);

$('#shelfSearch').addEventListener('input', (event) => {
  shelfQuery = event.currentTarget.value.trim();
  clearTimeout(shelfSearchTimer);
  shelfSearchTimer = setTimeout(loadBooks, 240);
});
$('#shelfSearch').addEventListener('search', (event) => {
  shelfQuery = event.currentTarget.value.trim();
  loadBooks();
});

$('#importForm').addEventListener('submit', async (event) => {
  event.preventDefault();
  const form = event.currentTarget;
  const url = form.elements.source_url.value.trim();
  const file = form.elements.audio_file.files[0];
  const epubFile = form.elements.epub_file.files[0];
  const message = $('#importError');
  const submit = $('#importSubmit');
  if (!!url === !!file) {
    message.textContent = 'Provide either a URL or an audio file, not both.';
    return;
  }
  const data = new FormData(form);
  if (!file) data.delete('audio_file');
  if (!url) data.delete('source_url');
  if (!epubFile) data.delete('epub_file');
  submit.disabled = true;
  submit.textContent = 'Adding…';
  message.textContent = 'Upload may take a little while. You can start reading as soon as the first section is ready.';
  try {
    const book = await api('/api/books', { method: 'POST', body: data });
    form.reset();
    $('#addDialog').close();
    await loadBooks();
    if (book?.id) location.href = `/reader/${encodeURIComponent(book.id)}`;
  } catch (error) {
    message.textContent = error.message || 'Could not add this book.';
  } finally {
    submit.disabled = false;
    submit.textContent = 'Add to bookshelf';
  }
});

$('#closeDiscover').addEventListener('click', () => $('#discoverDialog').close());
$('#pairBack').addEventListener('click', () => {
  $('#pairSearchForm').hidden = false;
  $('#pairSearchMessage').hidden = false;
  $('#pairConfirm').hidden = true;
  $('#pairResults').hidden = false;
  selectedPair = null;
});

function openDiscover() {
  selectedPair = null;
  pairResults = [];
  $('#pairConfirm').hidden = true;
  $('#pairSearchForm').hidden = false;
  $('#pairSearchMessage').hidden = false;
  $('#pairResults').hidden = false;
  $('#pairSearchMessage').textContent = '';
  $('#pairImportMessage').textContent = '';
  $('#pairResults').replaceChildren();
  $('#rightsConfirmed').checked = false;
  $('#discoverDialog').showModal();
  $('#pairSearch').focus();
}

$('#pairSearchForm').addEventListener('submit', async (event) => {
  event.preventDefault();
  const query = $('#pairSearch').value.trim();
  const message = $('#pairSearchMessage');
  const submit = $('#pairSearchSubmit');
  if (query.length < 2) {
    message.textContent = 'Enter at least two characters.';
    return;
  }
  submit.disabled = true;
  submit.textContent = 'Searching…';
  message.textContent = 'Searching source-linked LibriVox and Gutenberg records…';
  $('#pairResults').replaceChildren();
  try {
    const params = new URLSearchParams({ q: query });
    pairResults = (await api(`/api/discovery/pairs?${params}`)) || [];
    message.textContent = pairResults.length
      ? `${pairResults.length} source-linked ${pairResults.length === 1 ? 'pair' : 'pairs'} found.`
      : 'No source-linked pairs found. Try a different title.';
    $('#pairResults').innerHTML = pairResults.map((pair) => {
      const authors = (pair.authors || []).join(', ');
      const meta = [authors, pair.language, formatDuration(pair.duration_ms)].filter(Boolean).join(' · ');
      return `<article class="pair-result">
        <h3>${escapeHTML(pair.title)}</h3>
        <p class="muted">${escapeHTML(meta || 'LibriVox audio · Project Gutenberg text')}</p>
        <div class="pair-result-actions">
          <a href="${escapeHTML(pair.gutenberg_url)}" target="_blank" rel="noopener noreferrer">View ebook source ↗</a>
          <button type="button" class="button button-quiet" data-pair-id="${escapeHTML(pair.record_id)}">Import pair</button>
        </div>
      </article>`;
    }).join('');
  } catch (error) {
    message.textContent = error.message || 'The paired-book catalog could not be searched.';
  } finally {
    submit.disabled = false;
    submit.textContent = 'Search';
  }
});

$('#pairResults').addEventListener('click', (event) => {
  const button = event.target.closest('[data-pair-id]');
  if (!button) return;
  selectedPair = pairResults.find((pair) => pair.record_id === button.dataset.pairId);
  if (!selectedPair) return;
  $('#pairTitle').textContent = selectedPair.title;
  $('#pairByline').textContent = [
    (selectedPair.authors || []).join(', '),
    selectedPair.language,
    formatDuration(selectedPair.duration_ms),
  ].filter(Boolean).join(' · ');
  $('#pairLibrivoxLink').href = selectedPair.librivox_url;
  $('#pairGutenbergLink').href = selectedPair.gutenberg_url;
  $('#pairImportMessage').textContent = '';
  $('#rightsConfirmed').checked = false;
  $('#pairResults').hidden = true;
  $('#pairSearchForm').hidden = true;
  $('#pairSearchMessage').hidden = true;
  $('#pairConfirm').hidden = false;
});

$('#pairImportSubmit').addEventListener('click', async (event) => {
  if (!selectedPair) return;
  const message = $('#pairImportMessage');
  const button = event.currentTarget;
  if (!$('#rightsConfirmed').checked) {
    message.textContent = 'Please confirm you may use these editions where you live.';
    return;
  }
  button.disabled = true;
  button.textContent = 'Importing…';
  message.textContent = 'Fetching the recording and EPUB, then preparing the synchronized reader…';
  try {
    const book = await api(`/api/discovery/pairs/${encodeURIComponent(selectedPair.record_id)}/import`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ rights_confirmed: true }),
    });
    $('#discoverDialog').close();
    await loadBooks();
    if (book?.id) location.href = `/reader/${encodeURIComponent(book.id)}`;
  } catch (error) {
    message.textContent = error.message || 'Could not import this pair.';
  } finally {
    button.disabled = false;
    button.textContent = 'Import audio + EPUB';
  }
});

$('#books').addEventListener('click', async (event) => {
  const retranscribeButton = event.target.closest('[data-retranscribe-id]');
  if (retranscribeButton) {
    event.preventDefault();
    event.stopPropagation();
    if (retranscribeButton.disabled) return;
    retranscriptionTarget = {
      id: retranscribeButton.dataset.retranscribeId,
      title: retranscribeButton.dataset.retranscribeTitle || 'this book',
    };
    $('#retranscribeTitle').textContent = retranscriptionTarget.title;
    $('#retranscribeError').textContent = '';
    $('#confirmRetranscribe').disabled = false;
    $('#confirmRetranscribe').textContent = 'Re-transcribe';
    $('#retranscribeDialog').showModal();
    return;
  }
  const button = event.target.closest('[data-delete-id]');
  if (!button) return;
  event.preventDefault();
  event.stopPropagation();
  if (!confirm('Remove this book and its audio from your library?')) return;
  button.disabled = true;
  try {
    await api(`/api/books/${encodeURIComponent(button.dataset.deleteId)}`, { method: 'DELETE' });
    await loadBooks();
  } catch (error) {
    alert(error.message || 'Could not remove this book.');
    button.disabled = false;
  }
});

function closeRetranscribeDialog() {
  retranscriptionTarget = null;
  $('#retranscribeDialog').close();
}

$('#retranscribeDialog').addEventListener('close', () => { retranscriptionTarget = null; });
$('#closeRetranscribe').addEventListener('click', closeRetranscribeDialog);
$('#cancelRetranscribe').addEventListener('click', closeRetranscribeDialog);
$('#confirmRetranscribe').addEventListener('click', async (event) => {
  if (!retranscriptionTarget) return;
  const button = event.currentTarget;
  const error = $('#retranscribeError');
  button.disabled = true;
  button.textContent = 'Queuing…';
  error.textContent = '';
  try {
    await api(`/api/books/${encodeURIComponent(retranscriptionTarget.id)}/retranscribe`, { method: 'POST' });
    closeRetranscribeDialog();
    await loadBooks();
  } catch (failure) {
    error.textContent = failure.message || 'Could not queue a fresh transcript.';
    button.disabled = false;
    button.textContent = 'Re-transcribe';
  }
});

$('#adminToggle').addEventListener('click', async () => {
  $('#shelfPage').hidden = true;
  $('#adminPanel').hidden = false;
  await loadAdminUsers();
});
$('#adminBack').addEventListener('click', () => {
  $('#adminPanel').hidden = true;
  $('#shelfPage').hidden = false;
});
$('#adminUsers').addEventListener('click', async (event) => {
  const button = event.target.closest('[data-user-id]');
  if (!button) return;
  const status = button.dataset.status === 'active' ? 'suspended' : 'active';
  button.disabled = true;
  try {
    await api(`/api/admin/users/${encodeURIComponent(button.dataset.userId)}`, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ status }),
    });
    await loadAdminUsers();
  } catch {
    button.disabled = false;
    alert('Could not update this account.');
  }
});

async function loadAdminUsers() {
  const root = $('#adminUsers');
  root.innerHTML = '<p class="muted">Loading accounts…</p>';
  try {
    const users = await api('/api/admin/users');
    root.innerHTML = users.map((user) => `<article class="person-row">
      <div class="person-meta">
        <strong>${escapeHTML(user.email)}</strong>
        <span>${escapeHTML(user.role)} · ${escapeHTML(user.status)} · ${user.book_count} books</span>
      </div>
      <button class="button ${user.status === 'suspended' ? 'button-quiet' : 'button-danger'}"
        data-user-id="${escapeHTML(user.id)}" data-status="${escapeHTML(user.status)}">
        ${user.status === 'active' ? 'Suspend' : 'Reactivate'}
      </button>
    </article>`).join('') || '<p class="muted">No accounts yet.</p>';
  } catch {
    root.innerHTML = '<p class="muted">Account list is unavailable.</p>';
  }
}

if ('serviceWorker' in navigator) navigator.serviceWorker.register('/sw.js').catch(console.warn);
boot();
setInterval(() => {
  if (!document.hidden && !$('#shelfPage').hidden) loadBooks();
}, 5000);
