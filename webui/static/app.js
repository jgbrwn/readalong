const $ = (selector) => document.querySelector(selector);
let shelfQuery = '';
let shelfSort = 'recent';
let shelfSequence = 0;
let shelfSearchTimer = 0;
let pairResults = [];
let selectedPair = null;
let retranscriptionTarget = null;
let coverAIModels = [];
let adminUsersCache = [];
let shelfBooksByID = new Map();
let coverReviewTarget = null;

async function apiResponse(path, options = {}) {
  const response = await fetch(path, { ...options, cache: 'no-store' });
  if (!response.ok) {
    const detail = (await response.text()).trim();
    throw new Error(detail || `Request failed (${response.status})`);
  }
  const body = await response.text();
  return {
    data: body.trim() ? JSON.parse(body) : null,
    headers: response.headers,
  };
}

async function api(path, options = {}) {
  return (await apiResponse(path, options)).data;
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
  if (book.stage === 'validating_ebook') return 'Checking selected Gutenberg text…';
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
    params.set('sort', shelfSort);
    const query = params.toString();
    const books = (await api(`/api/books${query ? `?${query}` : ''}`)) || [];
    if (request !== shelfSequence) return;
    shelfBooksByID = new Map(books.map((book) => [book.id, book]));
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
      const yearLabel = book.cover_year
        ? `${book.cover_kind === 'epub' ? 'Edition' : 'First published'} ${book.cover_year}`
        : '';
      const coverCredit = book.cover_kind === 'catalog' ? 'Open Library cover' : '';
      const subtitle = [book.author, yearLabel, coverCredit, formatDuration(book.duration_ms)].filter(Boolean).join(' · ');
      const fallbackCover = `<div class="book-cover cover-${coverColor(title)}"${book.cover_url ? ' hidden' : ''}>
        <span class="cover-kicker">${book.source_kind === 'youtube' ? 'AUDIOBOOK' : 'READALONG'}</span>
        <strong>${escapeHTML(title)}</strong>
        <span class="cover-author">${escapeHTML(book.author || '')}</span>
      </div>`;
      const coverMarkup = `<div class="book-cover-frame">
        ${book.cover_url ? `<img class="book-cover-image" data-cover-image src="${escapeHTML(book.cover_url)}" alt="Cover for ${escapeHTML(title)}" loading="lazy">` : ''}
        ${fallbackCover}
      </div>`;
      const jobActive = book.job_status === 'queued' || book.job_status === 'running';
      const coverRegenerating = Boolean(book.cover_regeneration_queued) || book.cover_status === 'checking';
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
          ${coverMarkup}
          <div class="book-info">
            <div class="book-state"><span class="state-dot state-${escapeHTML(book.status)}"></span>${escapeHTML(progressLabel(book))}</div>
            <h3>${escapeHTML(title)}</h3>
            <p>${escapeHTML([subtitle, book.mode === 'aligned' ? 'Audio + EPUB' : 'Transcript'].filter(Boolean).join(' · '))}</p>
            <div class="progress-track"><span style="width:${percentage}%"></span></div>
          </div>
        </a>
        ${book.cover_review_needed ? `<button class="cover-review-hint" type="button" data-cover-review-id="${escapeHTML(book.id)}">Cover ready · choose</button>` : ''}
        ${book.status === 'ready' ? `<div class="book-card-actions">
          <button class="retranscribe-book" type="button" data-retranscribe-id="${escapeHTML(book.id)}"
            data-retranscribe-title="${escapeHTML(title)}" ${jobActive ? 'disabled' : ''}>
            ${escapeHTML(retranscribeLabel)}
          </button>
          <button class="regenerate-cover" type="button" data-cover-regenerate-id="${escapeHTML(book.id)}"
            ${jobActive || coverRegenerating ? 'disabled' : ''}>
            ${coverRegenerating ? 'Checking cover…' : 'Regenerate cover'}
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
    const savedSort = localStorage.getItem('readalong-shelf-sort');
    if (['recent', 'title', 'author', 'lastread', 'progress'].includes(savedSort)) shelfSort = savedSort;
  } catch { /* Storage may be disabled; the default sort still works. */ }
  $('#shelfSort').value = shelfSort;
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
$('#shelfSort').addEventListener('change', (event) => {
  shelfSort = event.currentTarget.value;
  try { localStorage.setItem('readalong-shelf-sort', shelfSort); } catch { /* Preference is optional. */ }
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
    message.dataset.state = 'error';
    return;
  }
  submit.disabled = true;
  submit.textContent = 'Searching…';
  message.textContent = 'Searching free LibriVox audio and matching Gutenberg texts…';
  message.dataset.state = 'loading';
  $('#pairResults').replaceChildren();
  try {
    const params = new URLSearchParams({ q: query });
    const search = await apiResponse(`/api/discovery/pairs?${params}`);
    pairResults = search.data || [];
    const warning = search.headers.get('X-Readalong-Search-Warning');
    message.textContent = pairResults.length
      ? `${pairResults.length} free LibriVox ${pairResults.length === 1 ? 'match' : 'matches'} found. Review the text link before importing.${warning ? ` ${warning}` : ''}`
      : warning || 'No matching LibriVox audio + Gutenberg pairs found. Try a different title.';
    message.dataset.state = pairResults.length ? 'success' : 'empty';
    $('#pairResults').innerHTML = pairResults.map((pair) => {
      const authors = (pair.authors || []).join(', ');
      const provider = pair.provider === 'internet_archive' ? 'Internet Archive · LibriVox' : 'LibriVox';
      const match = pair.match_kind === 'title_author' ? 'Title + author suggestion' : 'Source-linked text';
      const meta = [provider, authors, pair.narrator ? `Read by ${pair.narrator}` : '', pair.language, formatDuration(pair.duration_ms)]
        .filter(Boolean).join(' · ');
      const candidateCount = (pair.text_candidates || []).length;
      return `<article class="pair-result">
        <h3>${escapeHTML(pair.title)}</h3>
        <p class="muted">${escapeHTML(meta || 'LibriVox audio')}</p>
        <p class="muted">${escapeHTML(match)}${candidateCount > 1 ? ` · ${candidateCount} Gutenberg editions` : ''}</p>
        <div class="pair-result-actions">
          <a href="${escapeHTML(pair.audio_source_url || pair.librivox_url || '#')}" target="_blank" rel="noopener noreferrer">View audio source ↗</a>
          <button type="button" class="button button-quiet" data-pair-id="${escapeHTML(pair.record_id)}">Review text</button>
        </div>
      </article>`;
    }).join('');
  } catch (error) {
    message.textContent = error.message || 'The paired-book catalog could not be searched.';
    message.dataset.state = 'error';
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
  const candidates = selectedPair.text_candidates || (selectedPair.gutenberg_id ? [{
    gutenberg_id: selectedPair.gutenberg_id,
    title: selectedPair.title,
    gutenberg_url: selectedPair.gutenberg_url,
    match_basis: 'source_linked',
  }] : []);
  const requireMatchConfirmation = selectedPair.match_kind === 'title_author' || candidates.length > 1;
  const textChoice = $('#pairTextChoice');
  textChoice.replaceChildren();
  for (const candidate of candidates) {
    const option = document.createElement('option');
    option.value = candidate.gutenberg_id;
    const issued = candidate.issued ? ` · catalog ${candidate.issued.slice(0, 4)}` : '';
    option.textContent = `${candidate.title || `Gutenberg ${candidate.gutenberg_id}`} · #${candidate.gutenberg_id}${issued}${candidate.author ? ` · ${candidate.author}` : ''}`;
    textChoice.append(option);
  }
  const ambiguous = candidates.length > 1;
  $('#pairTextChoiceWrap').hidden = !ambiguous;
  if (ambiguous) {
    const choose = document.createElement('option');
    choose.value = '';
    choose.textContent = 'Choose an edition…';
    choose.selected = true;
    textChoice.prepend(choose);
  }
  textChoice.value = candidates.length === 1 ? candidates[0].gutenberg_id : '';
  $('#pairTitle').textContent = selectedPair.title;
  $('#pairByline').textContent = [
    (selectedPair.authors || []).join(', '),
    selectedPair.narrator ? `Read by ${selectedPair.narrator}` : '',
    selectedPair.language,
    formatDuration(selectedPair.duration_ms),
  ].filter(Boolean).join(' · ');
  const audioLink = $('#pairAudioLink');
  audioLink.href = selectedPair.audio_source_url || selectedPair.librivox_url || '#';
  audioLink.textContent = selectedPair.provider === 'internet_archive'
    ? 'View Internet Archive audio item ↗'
    : 'View LibriVox recording details ↗';
  const libriVoxLink = $('#pairLibriVoxLink');
  libriVoxLink.hidden = selectedPair.provider !== 'internet_archive' || !selectedPair.librivox_url;
  libriVoxLink.href = selectedPair.librivox_url || '#';
  $('#pairMatchNote').textContent = selectedPair.match_note || 'A Gutenberg ID is linked in the source metadata. Edition match is not verified.';
  $('#pairMatchCheckWrap').hidden = !requireMatchConfirmation;
  $('#pairMatchConfirmed').checked = false;
  const gutenbergLink = $('#pairGutenbergLink');
  const selectedCandidate = candidates.find((candidate) => candidate.gutenberg_id === textChoice.value);
  gutenbergLink.href = selectedCandidate?.gutenberg_url || selectedPair.gutenberg_url || '#';
  gutenbergLink.textContent = selectedCandidate
    ? `View Gutenberg #${selectedCandidate.gutenberg_id} ↗`
    : 'Choose a Gutenberg text above ↗';
  $('#pairImportMessage').textContent = '';
  $('#rightsConfirmed').checked = false;
  $('#pairResults').hidden = true;
  $('#pairSearchForm').hidden = true;
  $('#pairSearchMessage').hidden = true;
  $('#pairConfirm').hidden = false;
});

$('#pairTextChoice').addEventListener('change', () => {
  const candidate = (selectedPair?.text_candidates || []).find((item) => item.gutenberg_id === $('#pairTextChoice').value);
  const link = $('#pairGutenbergLink');
  link.href = candidate?.gutenberg_url || '#';
  link.textContent = candidate ? `View Gutenberg #${candidate.gutenberg_id} ↗` : 'Choose a Gutenberg text above ↗';
});

$('#pairImportSubmit').addEventListener('click', async (event) => {
  if (!selectedPair) return;
  const message = $('#pairImportMessage');
  const button = event.currentTarget;
  if (!$('#rightsConfirmed').checked) {
    message.textContent = 'Please confirm you may use these editions where you live.';
    return;
  }
  const candidates = selectedPair.text_candidates || [];
  const gutenbergID = $('#pairTextChoice').value || selectedPair.gutenberg_id ||
    (candidates.length === 1 ? candidates[0].gutenberg_id : '');
  if (!gutenbergID) {
    message.textContent = 'Choose a Gutenberg edition to pair with this recording.';
    return;
  }
  if ((selectedPair.match_kind === 'title_author' || candidates.length > 1) && !$('#pairMatchConfirmed').checked) {
    message.textContent = 'Please confirm you reviewed the suggested text match.';
    return;
  }
  button.disabled = true;
  button.textContent = 'Importing…';
  message.textContent = 'Fetching the recording and EPUB, then preparing the synchronized reader…';
  try {
    const book = await api(`/api/discovery/pairs/${encodeURIComponent(selectedPair.record_id)}/import`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        rights_confirmed: true,
        gutenberg_id: gutenbergID,
        match_confirmed: $('#pairMatchConfirmed').checked,
      }),
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
  const coverButton = event.target.closest('[data-cover-review-id]');
  if (coverButton) {
    event.preventDefault();
    event.stopPropagation();
    openCoverReview(coverButton.dataset.coverReviewId);
    return;
  }
  const regenerateButton = event.target.closest('[data-cover-regenerate-id]');
  if (regenerateButton) {
    event.preventDefault();
    event.stopPropagation();
    if (regenerateButton.disabled) return;
    regenerateButton.disabled = true;
    regenerateButton.textContent = 'Queueing cover…';
    try {
      await api(`/api/books/${encodeURIComponent(regenerateButton.dataset.coverRegenerateId)}/cover-regenerate`, {
        method: 'POST',
      });
      await loadBooks();
    } catch (error) {
      alert(error.message || 'Could not queue cover regeneration.');
      regenerateButton.disabled = false;
      regenerateButton.textContent = 'Regenerate cover';
    }
    return;
  }
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

$('#books').addEventListener('error', (event) => {
  if (!event.target.matches('[data-cover-image]')) return;
  event.target.hidden = true;
  const fallback = event.target.parentElement.querySelector('.book-cover');
  if (fallback) fallback.hidden = false;
}, true);

function openCoverReview(bookID) {
  const book = shelfBooksByID.get(bookID);
  if (!book?.cover_review_needed) return;
  coverReviewTarget = book;
  $('#coverReviewTitle').textContent = book.title || 'Book cover';
  $('#coverReviewMessage').textContent = '';
  $('#coverReviewMessage').dataset.state = '';
  const currentImage = $('#coverReviewCurrentImage');
  const currentFallback = $('#coverReviewCurrentFallback');
  if (book.cover_url) {
    currentImage.src = book.cover_url;
    currentImage.alt = `Current cover for ${book.title || 'book'}`;
    currentImage.hidden = false;
    currentFallback.hidden = true;
  } else {
    currentImage.removeAttribute('src');
    currentImage.hidden = true;
    currentFallback.hidden = false;
    currentFallback.innerHTML = `<span class="cover-kicker">READALONG</span>
      <strong>${escapeHTML(book.title || 'Untitled')}</strong>
      <span class="cover-author">${escapeHTML(book.author || '')}</span>`;
  }
  const candidateImage = $('#coverReviewCandidateImage');
  const hasCatalog = Boolean(book.cover_candidate_url);
  $('#coverReviewCatalogOption').hidden = !hasCatalog;
  if (hasCatalog) {
    candidateImage.src = book.cover_candidate_url;
    candidateImage.alt = `Catalog cover suggestion for ${book.title || 'book'}`;
    candidateImage.hidden = false;
  } else {
    candidateImage.removeAttribute('src');
  }
  const hasAI = Boolean(book.cover_ai_candidate_url);
  $('#coverReviewAIOption').hidden = !hasAI;
  const aiImage = $('#coverReviewAIImage');
  if (hasAI) {
    aiImage.src = book.cover_ai_candidate_url;
    aiImage.alt = `Newly generated cover for ${book.title || 'book'}`;
    aiImage.hidden = false;
  } else {
    aiImage.removeAttribute('src');
  }
  const currentSource = book.cover_kind === 'epub'
    ? 'Imported EPUB'
    : book.cover_kind === 'audio'
      ? 'Embedded audio artwork'
      : coverProviderLabel(book.cover_provider);
  const currentYear = book.cover_year
    ? `${book.cover_kind === 'epub' ? 'Edition' : 'First published'} ${book.cover_year}`
    : '';
  $('#coverReviewCurrentCaption').textContent = book.cover_kind === 'ai_svg'
    ? `AI-designed Readalong cover${currentYear ? ` · ${currentYear}` : ''}`
    : book.cover_kind ? [`Current cover · ${currentSource}`, currentYear].filter(Boolean).join(' · ') : 'Current Readalong design';
  if (hasCatalog) {
    $('#coverReviewCandidateCaption').textContent = [
      'Catalog suggestion',
      coverProviderLabel(book.cover_candidate_provider),
      book.cover_candidate_year ? `First published ${book.cover_candidate_year}` : '',
    ].filter(Boolean).join(' · ');
  }
  $('#coverReviewAICaption').textContent = [
    'New Readalong SVG design',
    book.cover_ai_candidate_year ? `First published ${book.cover_ai_candidate_year}` : '',
  ].filter(Boolean).join(' · ');
  $('#keepCoverReview').textContent = book.cover_kind ? 'Keep current' : 'Not now';
  $('#useCoverCandidate').hidden = !hasCatalog;
  $('#useAICoverCandidate').hidden = !hasAI;
  $('#coverReviewDialog').showModal();
}

function coverProviderLabel(provider) {
  if (!provider) return 'Open Library';
  try {
    const hostname = new URL(provider).hostname.toLowerCase();
    if (hostname === 'openlibrary.org') return 'Open Library';
  } catch { /* Provider strings may be labels rather than URLs. */ }
  return String(provider).replace(/^https?:\/\//, '').replace(/\/.*$/, '');
}

$('#closeCoverReview').addEventListener('click', () => $('#coverReviewDialog').close());
$('#coverReviewDialog').addEventListener('close', () => { coverReviewTarget = null; });
$('#coverReviewDialog').addEventListener('error', (event) => {
  if (event.target.id === 'coverReviewCurrentImage') {
    event.target.hidden = true;
    $('#coverReviewCurrentFallback').hidden = false;
  } else if (event.target.id === 'coverReviewCandidateImage') {
    event.target.hidden = true;
    $('#coverReviewCandidateCaption').textContent = 'Catalog suggestion preview is temporarily unavailable';
  } else if (event.target.id === 'coverReviewAIImage') {
    event.target.hidden = true;
    $('#coverReviewAICaption').textContent = 'Readalong cover preview is temporarily unavailable';
  }
}, true);
$('#keepCoverReview').addEventListener('click', async () => {
  if (!coverReviewTarget?.cover_kind) {
    $('#coverReviewDialog').close();
    return;
  }
  await saveCoverChoice('keep_current');
});
$('#useCoverCandidate').addEventListener('click', () => saveCoverChoice('use_candidate'));
$('#useAICoverCandidate').addEventListener('click', () => saveCoverChoice('use_ai_candidate'));

async function saveCoverChoice(action) {
  if (!coverReviewTarget) return;
  const button = action === 'use_candidate' ? $('#useCoverCandidate')
    : action === 'use_ai_candidate' ? $('#useAICoverCandidate') : $('#keepCoverReview');
  const message = $('#coverReviewMessage');
  button.disabled = true;
  message.textContent = 'Saving your cover choice…';
  message.dataset.state = 'loading';
  try {
    await api(`/api/books/${encodeURIComponent(coverReviewTarget.id)}/cover-choice`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ action }),
    });
    $('#coverReviewDialog').close();
    await loadBooks();
  } catch (error) {
    message.textContent = error.message || 'Could not save this cover choice.';
    message.dataset.state = 'error';
  } finally {
    button.disabled = false;
  }
}

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
  await showAdminTab('people');
});

$('#adminTabs').addEventListener('click', async (event) => {
  const button = event.target.closest('[data-admin-tab]');
  if (button) await showAdminTab(button.dataset.adminTab);
});

async function showAdminTab(tab) {
  const sections = {
    people: '#adminPeople',
    coverAI: '#adminCoverAI',
    bookTransfer: '#adminBookTransfer',
  };
  if (!sections[tab]) tab = 'people';
  for (const [name, selector] of Object.entries(sections)) {
    $(selector).hidden = name !== tab;
  }
  for (const button of $('#adminTabs').querySelectorAll('[data-admin-tab]')) {
    const active = button.dataset.adminTab === tab;
    button.classList.toggle('is-active', active);
    button.setAttribute('aria-current', active ? 'page' : 'false');
  }
  if (tab === 'people') await loadAdminUsers();
  if (tab === 'coverAI') await loadCoverAISettings();
  if (tab === 'bookTransfer') await loadAdminBookTools();
}

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
    adminUsersCache = users;
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

$('#adminBookSourceUser').addEventListener('change', async () => {
  populateAdminBookRecipients();
  await loadAdminBooksForSource();
});
$('#adminBookTargetUser').addEventListener('change', updateAdminBookOperationButton);
$('#adminBookSelect').addEventListener('change', updateAdminBookOperationButton);
$('#adminBookMode').addEventListener('change', updateAdminBookOperationButton);

async function loadAdminBookTools() {
  const status = $('#adminBookOperationMessage');
  status.textContent = 'Loading account books…';
  status.dataset.state = 'loading';
  if (!adminUsersCache.length) {
    try {
      adminUsersCache = await api('/api/admin/users');
    } catch (error) {
      status.textContent = error.message || 'Account list is unavailable.';
      status.dataset.state = 'error';
      return;
    }
  }
  const source = $('#adminBookSourceUser');
  const previousSource = source.value;
  source.replaceChildren();
  for (const user of adminUsersCache) {
    source.add(new Option(`${user.email} · ${user.book_count} books`, user.id));
  }
  if (previousSource && adminUsersCache.some((user) => user.id === previousSource)) source.value = previousSource;
  populateAdminBookRecipients();
  await loadAdminBooksForSource();
  status.textContent = 'Choose a ready book and an active recipient account.';
  status.dataset.state = 'empty';
}

function populateAdminBookRecipients() {
  const sourceID = $('#adminBookSourceUser').value;
  const select = $('#adminBookTargetUser');
  const previous = select.value;
  select.replaceChildren();
  const recipients = adminUsersCache.filter((user) => user.status === 'active' && user.id !== sourceID);
  for (const user of recipients) select.add(new Option(`${user.email} · ${user.book_count} books`, user.id));
  if (previous && recipients.some((user) => user.id === previous)) select.value = previous;
  updateAdminBookOperationButton();
}

async function loadAdminBooksForSource() {
  const select = $('#adminBookSelect');
  const sourceID = $('#adminBookSourceUser').value;
  select.replaceChildren();
  select.disabled = true;
  if (!sourceID) {
    select.add(new Option('Choose a source account', ''));
    updateAdminBookOperationButton();
    return;
  }
  select.add(new Option('Loading books…', ''));
  try {
    const books = await api(`/api/admin/users/${encodeURIComponent(sourceID)}/books`);
    const eligible = books.filter((book) => book.status === 'ready' &&
      book.job_status !== 'queued' && book.job_status !== 'running');
    select.replaceChildren();
    if (!eligible.length) {
      select.add(new Option('No ready books available', ''));
    } else {
      for (const book of eligible) {
        const subtitle = [book.author, book.mode === 'aligned' ? 'Audio + EPUB' : 'Transcript']
          .filter(Boolean).join(' · ');
        select.add(new Option(`${book.title || 'Untitled'}${subtitle ? ` · ${subtitle}` : ''}`, book.id));
      }
      select.disabled = false;
    }
  } catch (error) {
    select.replaceChildren(new Option(error.message || 'Books are unavailable', ''));
  }
  updateAdminBookOperationButton();
}

function updateAdminBookOperationButton() {
  const sourceID = $('#adminBookSourceUser').value;
  const targetID = $('#adminBookTargetUser').value;
  const bookID = $('#adminBookSelect').value;
  const mode = $('#adminBookMode').value;
  const button = $('#adminBookOperationSubmit');
  button.disabled = !sourceID || !targetID || sourceID === targetID || !bookID ||
    $('#adminBookSelect').disabled;
  button.textContent = mode === 'transfer' ? 'Transfer book' : 'Clone book';
}

$('#adminBookOperationForm').addEventListener('submit', async (event) => {
  event.preventDefault();
  const button = $('#adminBookOperationSubmit');
  const message = $('#adminBookOperationMessage');
  const request = {
    book_id: $('#adminBookSelect').value,
    source_user_id: $('#adminBookSourceUser').value,
    target_user_id: $('#adminBookTargetUser').value,
    mode: $('#adminBookMode').value,
  };
  const selectedBook = $('#adminBookSelect').selectedOptions[0]?.textContent || 'this book';
  const recipient = $('#adminBookTargetUser').selectedOptions[0]?.textContent || 'the selected account';
  const isTransfer = request.mode === 'transfer';
  const action = isTransfer ? 'Transfer' : 'Clone';
  const consequence = isTransfer ? ' The source account will lose access.' : ' The original remains in place.';
  if (!window.confirm(`${action} ${selectedBook} to ${recipient}?${consequence}`)) return;
  button.disabled = true;
  message.textContent = 'Queueing the secure file operation…';
  message.dataset.state = 'loading';
  try {
    const accepted = await api('/api/admin/book-operations', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(request),
    });
    let operation = accepted.operation;
    if (!operation?.id) throw new Error('The book operation was not accepted.');
    while (operation.status === 'queued' || operation.status === 'running') {
      await new Promise((resolve) => setTimeout(resolve, 1200));
      operation = await api(`/api/admin/book-operations/${encodeURIComponent(operation.id)}`);
      const copied = formatBytes(operation.copied_bytes);
      const total = formatBytes(operation.total_bytes);
      const progress = operation.total_bytes ? ` ${copied} of ${total}` : '';
      const stage = operation.stage === 'publishing' ? 'Publishing'
        : operation.stage === 'cleanup' ? 'Finalizing transfer'
          : 'Copying';
      message.textContent = `${stage}${progress}…`;
    }
    if (operation.status !== 'completed') throw new Error(operation.error || 'Book operation failed.');
    message.textContent = isTransfer
      ? 'Book transferred. The recipient starts with fresh reading progress.'
      : 'Book cloned. The original remains unchanged.';
    message.dataset.state = 'success';
    adminUsersCache = [];
    await loadAdminUsers();
    await loadAdminBookTools();
  } catch (error) {
    message.textContent = error.message || 'Could not copy this book.';
    message.dataset.state = 'error';
  } finally {
    updateAdminBookOperationButton();
  }
});

function formatBytes(bytes) {
  if (!Number.isFinite(bytes) || bytes < 0) return '';
  if (bytes < 1024 * 1024) return `${Math.round(bytes / 1024)} KB`;
  if (bytes < 1024 * 1024 * 1024) return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
  return `${(bytes / (1024 * 1024 * 1024)).toFixed(1)} GB`;
}

$('#coverAIModel').addEventListener('change', () => {
  const selected = coverAIModels.find((model) => model.id === $('#coverAIModel').value);
  if (selected) $('#coverAIAPIStyle').value = 'auto';
  updateCoverAIModelInfo(selected);
});

$('#coverAIModelFilter').addEventListener('input', () => {
  const selectedID = $('#coverAIModel').value;
  populateCoverAIModelOptions(selectedID, selectedID);
});

$('#refreshCoverModels').addEventListener('click', async (event) => {
  const button = event.currentTarget;
  button.disabled = true;
  $('#coverAIStatus').textContent = 'Checking Reflection for the current model catalog…';
  $('#coverAIStatus').dataset.state = 'loading';
  try {
    await api('/api/admin/cover-ai/refresh', { method: 'POST' });
    await loadCoverAISettings();
    $('#coverAIStatus').textContent = 'Model catalog refreshed.';
    $('#coverAIStatus').dataset.state = 'success';
  } catch (error) {
    $('#coverAIStatus').textContent = error.message || 'Could not refresh the model list.';
    $('#coverAIStatus').dataset.state = 'error';
  } finally {
    button.disabled = false;
  }
});

$('#checkCoverModel').addEventListener('click', async (event) => {
  const button = event.currentTarget;
  const modelID = $('#coverAIModel').value;
  if (!modelID) return;
  button.disabled = true;
  $('#coverAIStatus').textContent = 'Sending a small health-check request…';
  $('#coverAIStatus').dataset.state = 'loading';
  try {
    const result = await api('/api/admin/cover-ai/check', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ model_id: modelID, api_style: $('#coverAIAPIStyle').value }),
    });
    if (result.healthy && $('#coverAIAPIStyle').value === 'auto') {
      $('#coverAIAPIStyle').value = result.api_style;
      $('#coverAIStatus').textContent = `${result.message} Auto-selected this format; save settings to keep it. (${result.latency_ms} ms)`;
    } else {
      $('#coverAIStatus').textContent = `${result.healthy ? 'Healthy' : 'Check failed'} · ${result.message} (${result.latency_ms} ms)`;
    }
    $('#coverAIStatus').dataset.state = result.healthy ? 'success' : 'error';
  } catch (error) {
    $('#coverAIStatus').textContent = error.message || 'Could not check this model.';
    $('#coverAIStatus').dataset.state = 'error';
  } finally {
    button.disabled = false;
  }
});

$('#saveCoverAI').addEventListener('click', async (event) => {
  const button = event.currentTarget;
  button.disabled = true;
  $('#coverAIStatus').textContent = '';
  try {
    const settings = {
      enabled: $('#enableCoverAI').checked,
      catalog_lookup_enabled: $('#enableCoverLookup').checked,
      use_book_description: $('#includeCoverDescription').checked,
      model_id: $('#coverAIModel').value,
      api_style: $('#coverAIAPIStyle').value,
    };
    await api('/api/admin/cover-ai', {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(settings),
    });
    $('#coverAIStatus').textContent = 'Cover settings saved.';
    $('#coverAIStatus').dataset.state = 'success';
  } catch (error) {
    $('#coverAIStatus').textContent = error.message || 'Could not save cover settings.';
    $('#coverAIStatus').dataset.state = 'error';
  } finally {
    button.disabled = false;
  }
});

async function loadCoverAISettings() {
  const status = $('#coverAIStatus');
  const select = $('#coverAIModel');
  status.textContent = 'Loading managed models…';
  status.dataset.state = 'loading';
  try {
    const data = await api('/api/admin/cover-ai');
    coverAIModels = data.models || [];
    const selectedID = data.settings?.model_id || '';
    populateCoverAIModelOptions(selectedID, selectedID);
    $('#coverAIAPIStyle').value = ['auto', 'responses', 'chat_completions'].includes(data.settings?.api_style)
      ? data.settings.api_style : 'auto';
    $('#enableCoverAI').checked = Boolean(data.settings?.enabled);
    $('#enableCoverLookup').checked = data.settings?.catalog_lookup_enabled !== false;
    $('#includeCoverDescription').checked = data.settings?.use_book_description !== false;
    $('#checkCoverModel').disabled = !coverAIModels.some((model) => model.id === select.value && model.text_output);
    updateCoverAIModelInfo(coverAIModels.find((model) => model.id === select.value));
    if (data.warning) {
      status.textContent = data.warning;
      status.dataset.state = 'error';
    } else {
      status.textContent = data.fetched_at
        ? `Model list updated ${new Date(data.fetched_at).toLocaleString()}.`
        : 'No cached model list yet. Refresh to discover models.';
      status.dataset.state = data.fetched_at ? 'success' : 'empty';
    }
  } catch (error) {
    coverAIModels = [];
    select.replaceChildren(new Option('Model discovery unavailable', ''));
    select.disabled = true;
    $('#checkCoverModel').disabled = true;
    status.textContent = error.message || 'Could not load the managed model list.';
    status.dataset.state = 'error';
  }
}

function populateCoverAIModelOptions(preferredID = $('#coverAIModel').value, requiredID = '') {
  const select = $('#coverAIModel');
  const query = $('#coverAIModelFilter').value.trim().toLowerCase();
  const matches = coverAIModels.filter((model) => [
    model.id, model.name, model.gateway, model.provider, model.description,
  ].some((part) => String(part || '').toLowerCase().includes(query)));
  select.replaceChildren();
  if (!coverAIModels.length) {
    select.add(new Option('No text-generation models discovered', ''));
    select.disabled = true;
    return;
  }
  select.disabled = false;
  const groups = new Map();
  for (const model of matches) {
    const gateway = model.gateway || model.provider || 'Other managed models';
    if (!groups.has(gateway)) {
      const group = document.createElement('optgroup');
      group.label = gateway;
      select.append(group);
      groups.set(gateway, group);
    }
    groups.get(gateway).append(modelOption(model));
  }
  const current = coverAIModels.find((model) => model.id === requiredID);
  if (current && !matches.some((model) => model.id === current.id)) {
    const group = document.createElement('optgroup');
    group.label = 'Current selection';
    group.append(modelOption(current));
    select.append(group);
  } else if (requiredID && !current) {
    const group = document.createElement('optgroup');
    group.label = 'Saved selection';
    const unavailable = new Option(`${requiredID} · no longer in catalog`, requiredID);
    group.append(unavailable);
    select.append(group);
  }
  if (!matches.length && !requiredID) select.add(new Option('No models match this search', ''));
  if (preferredID && [...select.options].some((option) => option.value === preferredID)) {
    select.value = preferredID;
  } else if (!query && matches.length) {
    select.value = matches[0].id;
  } else {
    select.value = '';
  }
}

function modelOption(model) {
  const priceParts = [
    model.input_per_million == null ? '' : `$${Number(model.input_per_million).toFixed(3)}/1M input`,
    model.output_per_million == null ? '' : `$${Number(model.output_per_million).toFixed(3)}/1M output`,
  ].filter(Boolean);
  const price = priceParts.length ? priceParts.join(' · ')
    : model.gateway === 'OpenAI via exe.dev' ? 'price not listed · account billed'
      : 'price not listed';
  const provider = model.provider && model.provider !== model.gateway ? `${model.provider} · ` : '';
  const unverified = model.text_output_known ? '' : ' · check required';
  const preference = model.api_style === 'responses' ? 'Responses preferred' : 'Chat preferred';
  return new Option(`${provider}${model.name} · ${model.id} · ${preference} · ${price}${unverified}`, model.id);
}

function updateCoverAIModelInfo(model) {
  const info = $('#coverAIModelInfo');
  if (!model) {
    info.textContent = 'Choose a text-generating model. Readalong renders the final, safely typeset cover as SVG.';
    $('#checkCoverModel').disabled = true;
    return;
  }
  const capability = model.text_output_known
    ? 'The catalog advertises text output; Readalong uses it for a structured design recipe.'
    : 'This gateway does not publish text-capability metadata for this model; run Check model before enabling it.';
  const optional = [
    model.vision ? 'It also accepts image input, which this feature does not need.' : '',
    model.image_output ? 'It also advertises native image output; Readalong currently renders SVG locally.' : '',
    `Auto will try ${model.api_style === 'responses' ? 'Responses' : 'Chat Completions'} first.`,
  ].filter(Boolean).join(' ');
  info.textContent = [model.description, capability, optional].filter(Boolean).join(' ');
  $('#checkCoverModel').disabled = false;
}

if ('serviceWorker' in navigator) navigator.serviceWorker.register('/sw.js').catch(console.warn);
boot();
setInterval(() => {
  if (!document.hidden && !$('#shelfPage').hidden) loadBooks();
}, 5000);
