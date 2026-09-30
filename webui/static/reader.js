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
let loadingWindow = null;
let activeWord = -1;
let activeSentence = -1;
let animationFrame = 0;
let saveTimer = 0;
let appearanceSaveTimer = 0;
let progressRetryTimer = 0;
let lastSaveAttempt = 0;
let saveInFlight = false;
let saveAgain = false;
let appearanceUpdatedAt = 0;
let screenWakeLock = null;
let screenWakeRequest = null;
let ready = false;
let readerMode = 'transcript';
let savedAppearance = { font_size: 1.35, theme: 'light', highlight_mode: 'word' };
const progressSaveInterval = 12000;

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
  const dark = savedAppearance.theme === 'dark';
  document.body.classList.toggle('theme-dark', dark);
  document.body.style.setProperty('--reader-size', `${savedAppearance.font_size || 1.35}rem`);
  $('#highlightMode').value = savedAppearance.highlight_mode || 'word';
  $('#themeToggle').textContent = dark ? '☀' : '☾';
  $('#themeToggle').setAttribute('aria-label', dark ? 'Switch to light mode' : 'Switch to dark mode');
  $('#themeToggle').setAttribute('title', dark ? 'Switch to light mode' : 'Switch to dark mode');
  $('#themeToggle').setAttribute('aria-pressed', String(dark));
  document.querySelector('meta[name="theme-color"]')?.setAttribute('content', dark ? '#171d19' : '#fbfaf6');
  updateHighlight();
}

function persistAppearance() {
  appearanceUpdatedAt = Date.now();
  try {
    localStorage.setItem(appearanceStorageKey(), JSON.stringify({
      updated_at: appearanceUpdatedAt,
      appearance: savedAppearance,
    }));
  } catch { /* server persistence remains available if storage is disabled */ }
  queueAppearanceSave();
}

function appearanceStorageKey() {
  return `readalong:appearance:${bookID}`;
}

function readLocalAppearance() {
  try {
    const value = JSON.parse(localStorage.getItem(appearanceStorageKey()) || 'null');
    if (!value || !Number.isFinite(Number(value.updated_at)) ||
        !value.appearance || typeof value.appearance !== 'object') return null;
    return { updatedAt: Number(value.updated_at), appearance: value.appearance };
  } catch {
    return null;
  }
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

  let serverAppearance = {};
  try { serverAppearance = JSON.parse(book.appearance_json || '{}') || {}; } catch { /* old preference data */ }
  appearanceUpdatedAt = Number(serverAppearance._updated_at) || 0;
  delete serverAppearance._updated_at;
  const localAppearance = readLocalAppearance();
  const preferLocal = localAppearance && localAppearance.updatedAt > appearanceUpdatedAt;
  savedAppearance = {
    ...savedAppearance,
    ...(preferLocal ? localAppearance.appearance : serverAppearance),
  };
  if (preferLocal) appearanceUpdatedAt = localAppearance.updatedAt;
  applyAppearance();
  if (preferLocal) queueAppearanceSave();

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
  loadingWindow = { request, start };
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
    updateAlignmentNotice(data);
    chapters = data.chapters || chapters;
    const sentences = data.sentences || [];
    windowStart = Number(data.start_ms ?? start);
    windowEnd = Number(data.end_ms ?? end);
    renderSentences(sentences);
    ready = true;
    showError(book.error || '');
    if (readerMode === 'ebook') {
      const quality = Number(data.alignment_quality ?? book.alignment_quality) || 0;
      setStatus(quality >= 0.9
        ? 'Ebook text · strong match'
        : quality >= 0.75
          ? 'Ebook text · partial match'
          : `Ebook excerpt · very low match (${(quality * 100).toFixed(2)}%)`);
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
  } finally {
    if (loadingWindow?.request === request) loadingWindow = null;
  }
}

function statusLabel(value) {
  if (value?.stage === 'rate_limited') return 'Queued to resume';
  if (value?.stage === 'validating_ebook') return 'Checking selected ebook…';
  if (value?.stage === 'acquiring') return 'Finding audio…';
  if (value?.stage === 'normalizing') return 'Preparing audio…';
  if (value?.stage === 'transcribing') return 'Transcribing…';
  if (value?.status === 'error') return 'Processing stopped';
  return 'Preparing first section…';
}

function updateModeToggle(value = book) {
  const button = $('#readerModeToggle');
  const quality = Number(value?.alignment_quality) || 0;
  const hasEbook = value?.mode === 'aligned' && quality > 0;
  const weakMatch = hasEbook && quality < 0.75;
  button.hidden = !hasEbook;
  button.textContent = readerMode === 'ebook' ? 'Show transcript' : weakMatch ? 'Show EPUB excerpt' : 'Show EPUB';
  button.setAttribute('aria-label', readerMode === 'ebook'
    ? 'Show audio transcript'
    : weakMatch ? 'Show ebook excerpt; the audio match is very low' : 'Show ebook text');
  button.title = weakMatch
    ? 'Very little of this ebook matches the recording; only confident word matches are highlighted.'
    : '';
}

function updateAlignmentNotice(data) {
  const notice = $('#alignmentNotice');
  const quality = Number(data.alignment_quality ?? data.book?.alignment_quality) || 0;
  if (readerMode !== 'ebook' || quality >= 0.75) {
    notice.hidden = true;
    notice.textContent = '';
    return;
  }
  notice.hidden = false;
  notice.textContent = `Only ${(quality * 100).toFixed(2)}% of this EPUB matched the audio. This is a short excerpt around the current playback position; word highlights are sparse. The EPUB was processed, but this recording may be abridged or a different text.`;
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
  if (timeMS >= windowStart && timeMS < windowEnd) return;
  const targetStart = Math.max(0, Math.floor(Math.max(0, timeMS) / windowLength) * windowLength);
  if (loadingWindow?.start === targetStart) return;
  void loadWindow(timeMS);
}

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

function showWakeStatus(message, unavailable = false) {
  const status = $('#screenAwake');
  status.textContent = message;
  status.hidden = !message;
  status.title = message;
  status.setAttribute('aria-label', message);
  status.classList.toggle('unavailable', unavailable);
}

async function requestScreenWakeLock(allowWhileStarting = false) {
  if ((audio.paused && !allowWhileStarting) || document.visibilityState !== 'visible') return;
  if (screenWakeLock && !screenWakeLock.released) return;
  if (screenWakeRequest) return;
  if (!navigator.wakeLock?.request) {
    showWakeStatus('Screen may sleep', true);
    return;
  }
  const pending = navigator.wakeLock.request('screen');
  screenWakeRequest = pending;
  try {
    const lock = await pending;
    if ((audio.paused && !allowWhileStarting) || document.visibilityState !== 'visible') {
      await lock.release();
      return;
    }
    screenWakeLock = lock;
    showWakeStatus('Screen stays awake');
    lock.addEventListener('release', () => {
      if (screenWakeLock !== lock) return;
      screenWakeLock = null;
      if (!audio.paused && document.visibilityState === 'visible') {
        showWakeStatus('Screen may sleep', true);
      } else {
        showWakeStatus('');
      }
    });
  } catch {
    showWakeStatus(!audio.paused && document.visibilityState === 'visible' ? 'Screen may sleep' : '',
      !audio.paused && document.visibilityState === 'visible');
  } finally {
    if (screenWakeRequest === pending) screenWakeRequest = null;
  }
}

function releaseScreenWakeLock() {
  const lock = screenWakeLock;
  screenWakeLock = null;
  showWakeStatus('');
  if (lock && !lock.released) void lock.release().catch(() => {});
}

function scheduleSave(immediate = false) {
  if (immediate) {
    clearTimeout(saveTimer);
    saveTimer = 0;
    void saveProgress();
    return;
  }
  if (saveTimer) return;
  const delay = Math.max(0, progressSaveInterval - (Date.now() - lastSaveAttempt));
  saveTimer = setTimeout(() => {
    saveTimer = 0;
    void saveProgress();
  }, delay);
}

function queueAppearanceSave() {
  clearTimeout(appearanceSaveTimer);
  appearanceSaveTimer = setTimeout(() => {
    appearanceSaveTimer = 0;
    void saveProgress();
  }, 180);
}

async function saveProgress({ keepalive = false } = {}) {
  if (!bookID) return;
  if (saveInFlight && !keepalive) {
    saveAgain = true;
    return;
  }
  if (!keepalive) saveInFlight = true;
  lastSaveAttempt = Date.now();
  const appearance = {
    font_size: Math.min(2.4, Math.max(1, Number(savedAppearance.font_size) || 1.35)),
    theme: savedAppearance.theme || 'light',
    highlight_mode: ['word', 'sentence', 'minimal'].includes($('#highlightMode').value)
      ? $('#highlightMode').value
      : 'word',
    _updated_at: appearanceUpdatedAt,
  };
  try {
    await api(`/api/books/${encodeURIComponent(bookID)}/progress`, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      keepalive,
      body: JSON.stringify({
        position_ms: Math.max(0, Math.round((audio.currentTime || 0) * 1000)),
        playback_rate: audio.playbackRate || 1,
        sync_offset_ms: Number($('#syncOffset').value || 0),
        appearance,
      }),
    });
  } catch (error) {
    console.warn('Progress could not be saved:', error.message);
  } finally {
    if (!keepalive) {
      saveInFlight = false;
      if (saveAgain) {
        saveAgain = false;
        scheduleSave(true);
      }
    }
  }
}

function flushProgress() {
  clearTimeout(saveTimer);
  clearTimeout(appearanceSaveTimer);
  saveTimer = 0;
  appearanceSaveTimer = 0;
  void saveProgress({ keepalive: true });
}

function connectProgress() {
  if (!('EventSource' in window)) return;
  let sawActiveRetranscription = false;
  const stream = new EventSource(`/api/books/${encodeURIComponent(bookID)}/events`);
  stream.addEventListener('progress', async (event) => {
    let data;
    try { data = JSON.parse(event.data); } catch { return; }
    if (data.job_kind === 'retranscribe' &&
        (data.job_status === 'queued' || data.job_status === 'running')) {
      sawActiveRetranscription = true;
    }
    if (data.error) showError(data.error, data.job_status === 'error' && data.job_kind !== 'retranscribe');
    if (!ready) {
      setStatus(statusLabel(data));
      if (data.status === 'ready') {
        loadWindow(book?.position_ms || 0);
      }
    } else if (data.stage === 'transcribing' && data.status === 'ready') {
      setStatus('Ready · more words on the way');
    } else if (data.stage === 'retranscribing' && data.status === 'ready') {
      setStatus(`Refreshing transcript · ${Math.round((data.progress || 0) * 100)}% · current text remains available`);
    } else if (data.stage === 'aligning_retranscription') {
      setStatus('New transcript ready · aligning EPUB');
    } else if (data.stage === 'rate_limited' && data.status === 'ready') {
      setStatus('Groq rate-limited · current transcript remains available');
    }
    if (data.job_status === 'completed' || data.job_status === 'error') {
      stream.close();
      if (data.job_status === 'completed' && data.job_kind === 'retranscribe' && sawActiveRetranscription) {
        await refreshBookMode();
        setStatus('Fresh transcript ready');
        return;
      }
      const alignmentStillPending = !Number.isFinite(Number(book?.alignment_quality));
      if (data.job_status === 'completed' && book?.mode === 'aligned' && alignmentStillPending) {
        refreshBookMode();
      }
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
    const wakeLock = requestScreenWakeLock(true);
    try {
      await audio.play();
      await wakeLock;
    } catch {
      releaseScreenWakeLock();
      setStatus('Tap play to start audio');
    }
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
$('#seek').addEventListener('change', () => scheduleSave(true));
$('#playbackRate').addEventListener('change', (event) => {
  audio.playbackRate = Number(event.target.value);
  scheduleSave(true);
});
$('#syncOffset').addEventListener('input', () => { updateHighlight(); scheduleSave(); });
$('#syncOffset').addEventListener('change', persistAppearance);
$('#highlightMode').addEventListener('change', () => { updateHighlight(); persistAppearance(); });
$('#controlsToggle').addEventListener('click', (event) => {
  const panel = $('#expandedControls');
  const expanded = panel.hidden;
  panel.hidden = !expanded;
  event.currentTarget.setAttribute('aria-expanded', String(expanded));
  event.currentTarget.textContent = expanded ? 'Hide controls ↓' : 'Controls ↑';
});
$('#fontDown').addEventListener('click', () => {
  savedAppearance.font_size = Math.max(1, (Number(savedAppearance.font_size) || 1.35) - .1);
  applyAppearance(); persistAppearance();
});
$('#fontUp').addEventListener('click', () => {
  savedAppearance.font_size = Math.min(2.4, (Number(savedAppearance.font_size) || 1.35) + .1);
  applyAppearance(); persistAppearance();
});
$('#themeToggle').addEventListener('click', () => {
  savedAppearance.theme = savedAppearance.theme === 'dark' ? 'light' : 'dark';
  applyAppearance(); persistAppearance();
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
  void requestScreenWakeLock();
  startSyncLoop();
});
audio.addEventListener('pause', () => {
  $('#playPause').textContent = '▶';
  $('#playPause').setAttribute('aria-label', 'Play');
  cancelAnimationFrame(animationFrame);
  releaseScreenWakeLock();
  scheduleSave(true);
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
audio.addEventListener('ended', () => {
  releaseScreenWakeLock();
  scheduleSave(true);
});

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

document.addEventListener('visibilitychange', () => {
  if (document.visibilityState === 'hidden') {
    flushProgress();
    releaseScreenWakeLock();
    return;
  }
  updateClock();
  ensureWindow();
  updateHighlight();
  if (!audio.paused) {
    void requestScreenWakeLock();
    startSyncLoop();
  }
});
window.addEventListener('pagehide', () => {
  clearTimeout(progressRetryTimer);
  releaseScreenWakeLock();
  flushProgress();
});
if ('serviceWorker' in navigator) navigator.serviceWorker.register('/sw.js').catch(console.warn);
loadBook().catch((error) => {
  setStatus('Book unavailable');
  showError(error.message || 'This book could not be opened.');
});
