const $ = (selector) => document.querySelector(selector);

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
  if (book.error) return book.error;
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
  try {
    const books = (await api('/api/books')) || [];
    document.body.classList.toggle('has-books', books.length > 0);
    $('#heroAdd').textContent = books.length ? 'Add another book ↗' : 'Add your first book ↗';
    $('#bookCount').textContent = books.length ? `${books.length} ${books.length === 1 ? 'book' : 'books'}` : '';
    if (!books.length) {
      root.innerHTML = `<div class="empty-shelf">
        <div class="empty-mark" aria-hidden="true">↗</div>
        <h3>Your next chapter starts here.</h3>
        <p>Add an audiobook to see its words move with the story.</p>
        <button class="button button-dark" id="emptyAdd">Add a book</button>
      </div>`;
      $('#emptyAdd').onclick = openImport;
      return;
    }
    root.innerHTML = books.map((book) => {
      const title = book.title || 'Untitled';
      const percentage = Math.max(0, Math.min(100, Math.round((book.progress || 0) * 100)));
      const subtitle = [book.author, formatDuration(book.duration_ms)].filter(Boolean).join(' · ');
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
            <p>${escapeHTML(subtitle || 'Audio · transcript mode')}</p>
            <div class="progress-track"><span style="width:${percentage}%"></span></div>
          </div>
        </a>
        <button class="delete-book" type="button" data-delete-id="${escapeHTML(book.id)}" aria-label="Remove ${escapeHTML(title)}" title="Remove book">×</button>
      </article>`;
    }).join('');
  } catch (error) {
    root.innerHTML = `<p class="empty">${escapeHTML(error.message || 'Your bookshelf could not be loaded.')}</p>`;
  }
}

async function boot() {
  try {
    const user = await api('/api/me');
    $('#who').textContent = user.email || '';
    if (user.role === 'admin') $('#adminToggle').hidden = false;
  } catch {
    $('#who').textContent = 'Authentication required';
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

$('#importForm').addEventListener('submit', async (event) => {
  event.preventDefault();
  const form = event.currentTarget;
  const url = form.elements.source_url.value.trim();
  const file = form.elements.audio_file.files[0];
  const message = $('#importError');
  const submit = $('#importSubmit');
  if (!!url === !!file) {
    message.textContent = 'Provide either a URL or an audio file, not both.';
    return;
  }
  const data = new FormData(form);
  if (!file) data.delete('audio_file');
  if (!url) data.delete('source_url');
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

$('#books').addEventListener('click', async (event) => {
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
