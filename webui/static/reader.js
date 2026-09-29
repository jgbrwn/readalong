const $ = (selector) => document.querySelector(selector);
const bookID = decodeURIComponent(location.pathname.split('/').filter(Boolean).at(-1) || '');
const audio = $('#audio');
const windowLength = 5 * 60 * 1000;
const reducedMotion = matchMedia('(prefers-reduced-motion: reduce)').matches;

let book;
let chapters = [];
let activeWords = [];
let activeSentences = [];
let windowStart = 0;
let windowEnd = 0;
let requestSequence = 0;
let activeWord = -1;
let activeSentence = -1;
let animationFrame = 0;
let saveTimer = 0;
let progressRetryTimer = 0;
let ready = false;
let readerMode = 'transcript';
let savedAppearance = { font_size: 1.35, theme: 'light', highlight_mode: 'word' };

async function api(path, options = {}) {
  const response = await fetch(path, options);
  if (!response.ok) {
    const message = (await response.text()).trim();
    throw new Error(message || `Request failed (${response.status})`);
  }
  if (response.status === 204) return null;
  return response.json();
}

function formatTime(seconds) {
  if (!Number.isFinite(seconds) || seconds < 0) seconds = 0;
  const total = Math.floor(seconds);
  const hours = Math.floor(total / 3600);
  const minutes = Math.floor((total % 3600) / 60);
  const rest = total % 60;
  return hours
    ? `${hours}:${String(minutes).padStart(2, '0')}:${String(rest).padStart(2, '0')}`
    : `${minutes}:${String(rest).padStart(2, '0')}`;
}

function setStatus(text) {
  $('#readerStatus').textContent = text || '';
}

function showError(message, canRetry = false) {
  const box = $('#readerError');
  box.replaceChildren();
  box.hidden = !message;
  if (!message) return;
  const text = document.createElement('p');
  text.textContent = message;
  box.append(text);
  if (canRetry) {
    const button = document.createElement('button');
    button.className = 'button button-quiet';
    button.textContent = 'Retry processing';
    button.addEventListener('click', retry);
    box.append(button);
  }
}

function applyAppearance() {
  document.body.classList.toggle('theme-dark', savedAppearance.theme === 'dark');
  document.body.style.setProperty('--reader-size', `${savedAppearance.font_size || 1.35}rem`);
  $('#highlightMode').value = savedAppearance.highlight_mode || 'word';
  $('#themeToggle').setAttribute('aria-pressed', String(savedAppearance.theme === 'dark'));
  updateHighlight();
}

function persistAppearance() {
  scheduleSave();
}

async function loadBook() {
  const result = await api(`/api/books/${encodeURIComponent(bookID)}`);
  book = result.book;
  chapters = result.chapters || [];
  $('#bookTitle').textContent = book.title || 'Untitled';
  $('#bookAuthor').textContent = book.author || '';
  $('#totalTime').textContent = formatTime(book.duration_ms / 1000);
  $('#seek').max = String(book.duration_ms / 1000 || 0);
  readerMode = book.mode === 'aligned' && Number(book.alignment_quality) >= 0.75 ? 'ebook' : 'transcript';
  updateModeToggle(book);

  try {
    const appearance = JSON.parse(book.appearance_json || '{}');
    savedAppearance = { ...savedAppearance, ...appearance };
  } catch { /* ignore older or invalid preference data */ }
  applyAppearance();

  const rate = Number(book.playback_rate) || 1;
  if (![...$('#playbackRate').options].some((option) => Number(option.value) === rate)) {
    const option = document.createElement('option');
    option.value = String(rate);
    option.textContent = `${rate}×`;
    $('#playbackRate').append(option);
  }
  $('#playbackRate').value = String(rate);
  audio.playbackRate = rate;
  $('#syncOffset').value = String(book.sync_offset_ms || 0);
  if (book.error) showError(book.error, book.job_status === 'error');

  await loadWindow(book.position_ms || 0);
  if (ready) enablePlayer();
  connectProgress();
}

async function loadWindow(atMS) {
  const request = ++requestSequence;
  const start = Math.max(0, Math.floor(Math.max(0, atMS) / windowLength) * windowLength);
  const end = start + windowLength;
  setStatus('Loading words…');
  try {
    const query = new URLSearchParams({
      start_ms: String(start),
      end_ms: String(end),
      content: readerMode,
    });
    const data = await api(`/api/books/${encodeURIComponent(bookID)}/reader?${query}`);
    if (request !== requestSequence) return;
    if (!data.ready) {
      ready = false;
      setStatus(statusLabel(data.book));
      showError(data.book?.error || 'The first readable section is still being prepared.', data.book?.job_status === 'error');
      return;
    }
    book = data.book;
    readerMode = data.content_mode || readerMode;
    updateModeToggle(book);
    chapters = data.chapters || chapters;
    const sentences = data.sentences || [];
    windowStart = sentences[0]?.start_ms ?? data.start_ms;
    windowEnd = sentences.at(-1)?.end_ms ?? data.end_ms;
    renderSentences(sentences);
    ready = true;
    showError(book.error || '');
    if (readerMode === 'ebook') {
      const quality = Number(data.alignment_quality ?? book.alignment_quality) || 0;
      setStatus(quality >= 0.9 ? 'Ebook text · strong match' : 'Ebook text · partial match');
    } else if (book.mode === 'aligned' && data.alignment_pending) {
      setStatus('Transcript ready · aligning ebook');
    } else if (book.mode === 'aligned' && Number(book.alignment_quality) < 0.75) {
      setStatus('Transcript · low text match');
    } else {
      setStatus(book.stage === 'transcribing' ? 'Ready · more words on the way' : 'Ready to read');
    }
    enablePlayer();
    updateChapter();
    updateHighlight();
  } catch (error) {
    if (request === requestSequence) {
      setStatus('Reader unavailable');
      showError(error.message || 'Could not load this section.');
    }
  }
}

function statusLabel(value) {
  if (value?.stage === 'rate_limited') return 'Queued to resume';
  if (value?.stage === 'acquiring') return 'Finding audio…';
  if (value?.stage === 'normalizing') return 'Preparing audio…';
  if (value?.stage === 'transcribing') return 'Transcribing…';
  if (value?.status === 'error') return 'Processing stopped';
  return 'Preparing first section…';
}

function updateModeToggle(value = book) {
  const button = $('#readerModeToggle');
  const hasEbook = value?.mode === 'aligned' && Number(value.alignment_quality) > 0;
  button.hidden = !hasEbook;
  button.textContent = readerMode === 'ebook' ? 'Show transcript' : 'Show EPUB';
  button.setAttribute('aria-label', readerMode === 'ebook' ? 'Show audio transcript' : 'Show ebook text');
}

function renderSentences(sentences) {
  const root = $('#readerText');
  root.replaceChildren();
  activeWords = [];
  activeSentences = [];
  activeWord = -1;
  activeSentence = -1;
  lastScrolledSentence = -1;
  let paragraph;
  let previousParagraphID = null;
  for (const [sentenceOrdinal, sentence] of sentences.entries()) {
    const paragraphID = sentence.paragraph_id || sentence.id;
    if (!paragraph || paragraphID !== previousParagraphID) {
      paragraph = document.createElement('p');
      paragraph.className = 'reader-paragraph';
      root.append(paragraph);
      previousParagraphID = paragraphID;
    }
    const sentenceElement = document.createElement('span');
    sentenceElement.className = 'reader-sentence';
    sentenceElement.dataset.sentenceId = sentence.id;
    const sentenceIndex = activeSentences.length;
    activeSentences.push(sentenceElement);
    for (const [index, word] of sentence.words.entries()) {
      if (index) sentenceElement.append(document.createTextNode(' '));
      const span = document.createElement('span');
      span.className = 'reader-word';
      span.textContent = word.t;
      const start = Number(word.s);
      const end = Number(word.e);
      const confidence = Number(word.c) || 0;
      if (Number.isFinite(start) && Number.isFinite(end) && end > start && confidence >= 0.65) {
        span.dataset.start = String(start);
        span.dataset.end = String(end);
        span.dataset.wordIndex = String(activeWords.length);
        span.dataset.sentenceIndex = String(sentenceIndex);
        activeWords.push({ start, end, element: span, sentenceIndex });
      }
      sentenceElement.append(span);
    }
    paragraph.append(sentenceElement);
    if (sentenceOrdinal + 1 < sentences.length &&
        (sentences[sentenceOrdinal + 1].paragraph_id || sentences[sentenceOrdinal + 1].id) === paragraphID) {
      paragraph.append(document.createTextNode(' '));
    }
  }
}

function upperBoundWord(timeMS) {
  let low = 0, high = activeWords.length;
  while (low < high) {
    const mid = (low + high) >>> 1;
    if (activeWords[mid].start <= timeMS) low = mid + 1;
    else high = mid;
  }
  return low - 1;
}

function updateHighlight() {
  if (!ready || !activeWords.length) return;
  const timeMS = audio.currentTime * 1000 + Number($('#syncOffset').value || 0);
  let next = upperBoundWord(timeMS);
  if (next >= 0 && timeMS > activeWords[next].end + 1800) next = -1;
  const mode = $('#highlightMode').value;
  if (next === activeWord && next >= 0) {
    activeWords[next].element.classList.toggle('active-word', mode === 'word');
    activeSentences[activeWords[next].sentenceIndex].classList.toggle('active-sentence', mode !== 'minimal');
    activeSentences[activeWords[next].sentenceIndex].classList.toggle('minimal-active', mode === 'minimal');
    return;
  }

  if (activeWord >= 0) activeWords[activeWord].element.classList.remove('active-word');
  if (next > activeWord) {
    for (let i = Math.max(0, activeWord + 1); i < next; i++) activeWords[i].element.classList.add('read-word');
  } else if (next >= 0 && activeWord > next) {
    for (let i = next + 1; i <= activeWord; i++) activeWords[i].element.classList.remove('read-word');
  } else if (next < 0 && activeWord >= 0) {
    for (let i = 0; i <= activeWord; i++) activeWords[i].element.classList.remove('read-word');
  }

  const nextSentence = next >= 0 ? activeWords[next].sentenceIndex : -1;
  if (activeSentence >= 0 && activeSentences[activeSentence]) {
    activeSentences[activeSentence].classList.remove('active-sentence');
    activeSentences[activeSentence].classList.remove('minimal-active');
  }
  activeWord = next;
  activeSentence = nextSentence;

  if (next >= 0 && mode === 'word') activeWords[next].element.classList.add('active-word');
  if (nextSentence >= 0 && mode !== 'minimal') activeSentences[nextSentence].classList.add('active-sentence');
  if (nextSentence >= 0 && mode === 'minimal') activeSentences[nextSentence].classList.add('minimal-active');
  if (nextSentence >= 0 && nextSentence !== lastScrolledSentence) {
    lastScrolledSentence = nextSentence;
    activeSentences[nextSentence].scrollIntoView({
      behavior: reducedMotion ? 'auto' : 'smooth',
      block: 'center',
    });
  }
}
let lastScrolledSentence = -1;

function updateChapter() {
  const timeMS = audio.currentTime * 1000;
  const chapter = chapters.find((item) => timeMS >= item.start_ms && timeMS < item.end_ms);
  $('#chapterTitle').textContent = chapter?.title || '';
}

function updateClock() {
  $('#currentTime').textContent = formatTime(audio.currentTime);
  $('#seek').value = String(audio.currentTime || 0);
  updateChapter();
}

function ensureWindow() {
  if (!ready) return;
  const timeMS = audio.currentTime * 1000;
  const nearEnd = windowEnd > 0 && timeMS >= windowEnd - 20000 && windowEnd < book.duration_ms;
  if (timeMS < windowStart || timeMS >= windowEnd || nearEnd) {
    if (loadingAt !== null) return;
    const target = timeMS >= windowEnd - 20000 ? windowEnd : timeMS;
    loadingAt = target;
    loadWindow(target).finally(() => { loadingAt = null; });
  }
}
let loadingAt = null;

function frame() {
  updateHighlight();
  ensureWindow();
  if (!audio.paused && !audio.ended) animationFrame = requestAnimationFrame(frame);
}

function startSyncLoop() {
  cancelAnimationFrame(animationFrame);
  animationFrame = requestAnimationFrame(frame);
}

function enablePlayer() {
  if (!audio.src) {
    audio.src = `/api/books/${encodeURIComponent(bookID)}/audio`;
    audio.load();
  }
  $('#playPause').disabled = false;
  $('#seek').disabled = false;
}

function scheduleSave() {
  clearTimeout(saveTimer);
  saveTimer = setTimeout(saveProgress, 1800);
}

async function saveProgress() {
  if (!bookID) return;
  const appearance = {
    ...savedAppearance,
    font_size: Number(savedAppearance.font_size) || 1.35,
    theme: savedAppearance.theme || 'light',
    highlight_mode: $('#highlightMode').value || 'word',
  };
  savedAppearance = appearance;
  try {
    await api(`/api/books/${encodeURIComponent(bookID)}/progress`, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        position_ms: Math.max(0, Math.round((audio.currentTime || 0) * 1000)),
        playback_rate: audio.playbackRate || 1,
        sync_offset_ms: Number($('#syncOffset').value || 0),
        appearance,
      }),
    });
  } catch (error) {
    console.warn('Progress could not be saved:', error.message);
  }
}

function connectProgress() {
  if (!('EventSource' in window)) return;
  const stream = new EventSource(`/api/books/${encodeURIComponent(bookID)}/events`);
  stream.addEventListener('progress', (event) => {
    let data;
    try { data = JSON.parse(event.data); } catch { return; }
    if (data.error) showError(data.error, data.job_status === 'error');
    if (!ready) {
      setStatus(statusLabel(data));
      if (data.status === 'ready') {
        loadWindow(book?.position_ms || 0);
      }
    } else if (data.stage === 'transcribing' && data.status === 'ready') {
      setStatus('Ready · more words on the way');
    }
    if (data.job_status === 'completed' || data.job_status === 'error') {
      stream.close();
      if (data.job_status === 'completed' && book?.mode === 'aligned') refreshBookMode();
    } else if (data.stage === 'rate_limited') {
      stream.close();
      const retryAt = Date.parse(data.not_before_at || '');
      const delay = Number.isFinite(retryAt)
        ? Math.max(1000, Math.min(retryAt - Date.now(), 60000))
        : 60000;
      clearTimeout(progressRetryTimer);
      progressRetryTimer = setTimeout(connectProgress, delay);
    }
  });
  stream.onerror = () => {};
}

async function refreshBookMode() {
  try {
    const result = await api(`/api/books/${encodeURIComponent(bookID)}`);
    book = result.book;
    chapters = result.chapters || chapters;
    readerMode = Number(book.alignment_quality) >= 0.75 ? 'ebook' : 'transcript';
    updateModeToggle(book);
    await loadWindow(Math.max(0, Math.round(audio.currentTime * 1000)));
  } catch (error) {
    console.warn('Could not refresh paired ebook status:', error.message);
  }
}

async function retry() {
  try {
    await api(`/api/books/${encodeURIComponent(bookID)}/retry`, { method: 'POST' });
    showError('');
    setStatus('Retry queued…');
    connectProgress();
  } catch (error) {
    showError(error.message || 'Could not retry this book.');
  }
}

$('#playPause').addEventListener('click', async () => {
  if (!ready) return;
  if (audio.paused) {
    try { await audio.play(); } catch { setStatus('Tap play to start audio'); }
  } else {
    audio.pause();
  }
});
$('#readerModeToggle').addEventListener('click', () => {
  if (book?.mode !== 'aligned') return;
  readerMode = readerMode === 'ebook' ? 'transcript' : 'ebook';
  updateModeToggle(book);
  loadWindow(Math.max(0, Math.round(audio.currentTime * 1000)));
});
$('#back15').addEventListener('click', () => { audio.currentTime = Math.max(0, audio.currentTime - 15); });
$('#forward30').addEventListener('click', () => { audio.currentTime = Math.min(audio.duration || Infinity, audio.currentTime + 30); });
$('#seek').addEventListener('input', (event) => {
  if (Number.isFinite(audio.duration)) audio.currentTime = Number(event.target.value);
});
$('#seek').addEventListener('change', scheduleSave);
$('#playbackRate').addEventListener('change', (event) => {
  audio.playbackRate = Number(event.target.value);
  scheduleSave();
});
$('#syncOffset').addEventListener('input', () => { updateHighlight(); scheduleSave(); });
$('#syncOffset').addEventListener('change', persistAppearance);
$('#highlightMode').addEventListener('change', () => { updateHighlight(); scheduleSave(); });
$('#controlsToggle').addEventListener('click', (event) => {
  const panel = $('#expandedControls');
  const expanded = panel.hidden;
  panel.hidden = !expanded;
  event.currentTarget.setAttribute('aria-expanded', String(expanded));
  event.currentTarget.textContent = expanded ? 'Hide controls ↓' : 'Controls ↑';
});
$('#fontDown').addEventListener('click', () => {
  savedAppearance.font_size = Math.max(1, (Number(savedAppearance.font_size) || 1.35) - .1);
  applyAppearance(); scheduleSave();
});
$('#fontUp').addEventListener('click', () => {
  savedAppearance.font_size = Math.min(2.4, (Number(savedAppearance.font_size) || 1.35) + .1);
  applyAppearance(); scheduleSave();
});
$('#themeToggle').addEventListener('click', () => {
  savedAppearance.theme = savedAppearance.theme === 'dark' ? 'light' : 'dark';
  applyAppearance(); scheduleSave();
});
$('#readerText').addEventListener('click', (event) => {
  if (window.getSelection()?.toString()) return;
  const word = event.target.closest('[data-start]');
  if (!word || !ready) return;
  audio.currentTime = Number(word.dataset.start) / 1000;
});

audio.addEventListener('loadedmetadata', () => {
  if (book?.position_ms > 0 && audio.currentTime === 0) {
    audio.currentTime = Math.min(book.position_ms / 1000, audio.duration || Infinity);
  }
  updateClock();
});
audio.addEventListener('timeupdate', () => {
  updateClock();
  scheduleSave();
});
audio.addEventListener('play', () => {
  $('#playPause').textContent = 'Ⅱ';
  $('#playPause').setAttribute('aria-label', 'Pause');
  startSyncLoop();
});
audio.addEventListener('pause', () => {
  $('#playPause').textContent = '▶';
  $('#playPause').setAttribute('aria-label', 'Play');
  cancelAnimationFrame(animationFrame);
  scheduleSave();
});
audio.addEventListener('seeked', () => {
  updateClock();
  ensureWindow();
  updateHighlight();
});
audio.addEventListener('ratechange', () => {
  $('#playbackRate').value = String(audio.playbackRate);
  scheduleSave();
});
audio.addEventListener('ended', saveProgress);

document.addEventListener('keydown', (event) => {
  if (event.target.matches('input,select,textarea,button') || event.metaKey || event.ctrlKey) return;
  if (event.code === 'Space') {
    event.preventDefault();
    $('#playPause').click();
  } else if (event.code === 'ArrowLeft') {
    audio.currentTime = Math.max(0, audio.currentTime - 10);
  } else if (event.code === 'ArrowRight') {
    audio.currentTime = Math.min(audio.duration || Infinity, audio.currentTime + 10);
  }
});

window.addEventListener('pagehide', () => {
  clearTimeout(saveTimer);
  clearTimeout(progressRetryTimer);
  saveProgress();
});
if ('serviceWorker' in navigator) navigator.serviceWorker.register('/sw.js').catch(console.warn);
loadBook().catch((error) => {
  setStatus('Book unavailable');
  showError(error.message || 'This book could not be opened.');
});
