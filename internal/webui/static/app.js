// aakb static web UI — plain ES module, hash router, in-memory bearer token, WCAG 2.2 AA.

const MAX_BLOB_BYTES = 64 * 1024 * 1024; // 64 MiB
const SEARCH_ALLOWED_PARAMS = new Set([
  'q',
  'kind',
  'title',
  'author',
  'publisher',
  'year',
  'work_type',
  'format',
  'language',
  'source',
  'content',
  'sort',
  'page',
  'limit',
  'stats',
  'rank',
  'downloadable_only',
  'search_descriptions',
  'timeout_seconds',
]);

// Module-scoped state. The bearer token lives ONLY in this variable and is never
// stored in localStorage, sessionStorage, cookies, URLs, or the DOM.
let authToken = null;
let authMode = 'loading'; // 'loading' | 'tokenless' | 'token' | 'signin' | 'token_required' | 'error'
let capabilities = { version: '', openalex: false, auth_required: false };
let signInMessage = null;
let activeController = null;
let lastSearchHash = '#/';
let lastRecordHash = null;
const recordCache = new Map();

// Static shell references
const headerEl = document.getElementById('site-header');
const footerEl = document.getElementById('site-footer');
const versionEl = document.getElementById('app-version');
const navSearchEl = document.getElementById('nav-search');
const navAccountEl = document.getElementById('nav-account');
const signOutBtn = document.getElementById('sign-out-btn');
const appEl = document.getElementById('app');
const announcerEl = document.getElementById('sr-announcer');

// ---------------------------------------------------------------------------
// DOM and formatting helpers (no innerHTML, no inline styles)
// ---------------------------------------------------------------------------

function el(tag, props = null, ...children) {
  const node = document.createElement(tag);
  if (props) {
    for (const [key, val] of Object.entries(props)) {
      if (val === undefined || val === null || val === false) continue;
      if (key === 'className') {
        node.className = val;
      } else if (key === 'textContent') {
        node.textContent = String(val);
      } else if (key.startsWith('on') && typeof val === 'function') {
        node.addEventListener(key.slice(2).toLowerCase(), val);
      } else if (
        key === 'checked' ||
        key === 'disabled' ||
        key === 'hidden' ||
        key === 'open' ||
        key === 'required' ||
        key === 'selected' ||
        key === 'value' ||
        key === 'max'
      ) {
        node[key] = val;
      } else if (val === true) {
        node.setAttribute(key, '');
      } else {
        node.setAttribute(key, String(val));
      }
    }
  }
  for (const child of children.flat(Infinity)) {
    if (child === undefined || child === null || child === false) continue;
    if (typeof child === 'string' || typeof child === 'number') {
      node.appendChild(document.createTextNode(String(child)));
    } else {
      node.appendChild(child);
    }
  }
  return node;
}

function clear(node) {
  while (node.firstChild) {
    node.removeChild(node.firstChild);
  }
}

function announce(message) {
  if (!announcerEl) return;
  announcerEl.textContent = '';
  requestAnimationFrame(() => {
    announcerEl.textContent = message;
  });
}

function focusHeading(heading, pageTitle) {
  document.title = pageTitle ? `${pageTitle} — aakb` : 'aakb';
  if (heading) {
    heading.setAttribute('tabindex', '-1');
    heading.focus({ preventScroll: false });
  }
}

function safeExternalUrl(raw) {
  if (!raw || typeof raw !== 'string') return null;
  try {
    const parsed = new URL(raw);
    if (parsed.protocol === 'http:' || parsed.protocol === 'https:') {
      return parsed.href;
    }
  } catch {
    return null;
  }
  return null;
}

function safeDownloadUrl(raw) {
  if (!raw || typeof raw !== 'string') return null;
  if (!raw.startsWith('/downloads/')) return null;
  if (raw.includes('..') || raw.includes('\\')) return null;
  return raw;
}

function formatBytes(bytes) {
  if (typeof bytes !== 'number' || Number.isNaN(bytes) || bytes < 0) return '';
  if (bytes < 1024) return `${bytes} B`;
  const units = ['KiB', 'MiB', 'GiB', 'TiB'];
  let value = bytes / 1024;
  let idx = 0;
  while (value >= 1024 && idx < units.length - 1) {
    value /= 1024;
    idx += 1;
  }
  const formatted = value >= 10 ? value.toFixed(1) : value.toFixed(2);
  return `${formatted} ${units[idx]} (${bytes.toLocaleString()} bytes)`;
}

function isOpenAlexId(id) {
  return /^W\d+$/i.test(String(id || '').trim());
}

function isDOI(value) {
  return /^10\.\d{4,9}\/\S+$/i.test(String(value || '').trim());
}

function extractDOIFromDetails(details, fallbackRecord) {
  if (details && details.identifiers && typeof details.identifiers === 'object') {
    for (const [k, values] of Object.entries(details.identifiers)) {
      if (k.toLowerCase() === 'doi' && Array.isArray(values)) {
        const first = values.find((v) => typeof v === 'string' && v.trim() !== '');
        if (first) return first.trim();
      }
    }
  }
  if (details && isDOI(details.id)) return details.id.trim();
  if (fallbackRecord && typeof fallbackRecord.doi === 'string' && fallbackRecord.doi.trim()) {
    return fallbackRecord.doi.trim();
  }
  return null;
}

function recordIdentifier(rec) {
  if (!rec) return '';
  if (rec.hash && String(rec.hash).trim()) return String(rec.hash).trim();
  if (rec.id && String(rec.id).trim()) return String(rec.id).trim();
  if (rec.doi && String(rec.doi).trim()) return String(rec.doi).trim();
  return '';
}

function buildDownloadCliCommand(target) {
  const parts = ['aakb', 'download'];
  if (target.hash) {
    parts.push('-hash', target.hash);
  } else if (target.doi) {
    parts.push('-doi', target.doi);
  } else if (target.openalex_id) {
    parts.push('-openalex-id', target.openalex_id);
  }
  return parts.join(' ');
}

function formatSourceLabel(source) {
  const raw = String(source || '').trim();
  const isAbstract = /abstract/i.test(raw);
  const label = isAbstract ? 'Abstract' : 'Full text';
  return { label, raw, isAbstract };
}

// ---------------------------------------------------------------------------
// API client & RFC 9457 Problem JSON handling
// ---------------------------------------------------------------------------

class ApiError extends Error {
  constructor({ status = 0, code = '', title = '', detail = '', retryAfter = null } = {}) {
    super(detail || title || 'Request failed');
    this.name = 'ApiError';
    this.status = status;
    this.code = code;
    this.title = title;
    this.detail = detail;
    this.retryAfter = retryAfter;
  }
}

async function parseProblem(response) {
  const retryHeader = response.headers.get('Retry-After');
  let retryAfter = null;
  if (retryHeader && /^\d+$/.test(retryHeader.trim())) {
    retryAfter = Number.parseInt(retryHeader.trim(), 10);
  }

  let body = null;
  const contentType = (response.headers.get('Content-Type') || '').toLowerCase();
  if (contentType.includes('json')) {
    try {
      body = await response.json();
    } catch {
      body = null;
    }
  } else {
    try {
      const text = (await response.text()).trim();
      if (text) {
        return new ApiError({
          status: response.status,
          code: '',
          title: response.statusText || `HTTP ${response.status}`,
          detail: text,
          retryAfter,
        });
      }
    } catch {
      // Ignore read errors
    }
  }

  if (body && typeof body === 'object') {
    const parsedRetry =
      typeof body.retry_after === 'number' && Number.isFinite(body.retry_after)
        ? body.retry_after
        : retryAfter;
    return new ApiError({
      status: typeof body.status === 'number' ? body.status : response.status,
      code: typeof body.code === 'string' ? body.code : '',
      title: typeof body.title === 'string' ? body.title : response.statusText || `HTTP ${response.status}`,
      detail: typeof body.detail === 'string' ? body.detail : '',
      retryAfter: parsedRetry,
    });
  }

  return new ApiError({
    status: response.status,
    code: '',
    title: response.statusText || `HTTP ${response.status}`,
    detail: `Server returned status ${response.status}.`,
    retryAfter,
  });
}

function nextStepForError(err) {
  if (!(err instanceof ApiError)) {
    return 'Check that the aakb server is running and reachable, then try again.';
  }
  switch (err.code) {
    case 'UNAUTHORIZED':
      return 'Sign in again with the server HTTP token.';
    case 'TOKEN_REQUIRED':
      return 'Set AAKB_HTTP_TOKEN and restart the server, or bind the server to 127.0.0.1.';
    case 'BAD_HOST':
      return 'Open this app using localhost or 127.0.0.1 with the server port.';
    case 'RATE_LIMITED':
      if (err.retryAfter && err.retryAfter > 0) {
        return `Wait ${err.retryAfter} seconds before trying again.`;
      }
      return 'Wait a moment before trying again.';
    case 'CONFIG':
      return 'Check the server configuration (such as AAKB_ARCHIVE_SECRET_KEY or OPENALEX_API_KEY) and try again.';
    case 'INVALID_ARGUMENT':
      return 'Check your query or identifier and try again.';
    case 'NOT_FOUND':
    case 'ROUTE_NOT_FOUND':
      return 'Check the identifier or return to Search.';
    case 'REQUEST_TIMEOUT':
      return 'The upstream service timed out. Try again or narrow your query.';
    case 'UPSTREAM_BLOCKED':
    case 'UPSTREAM':
      return 'The upstream service is temporarily unavailable or blocked. Try again shortly.';
    default:
      if (err.status === 401) return 'Sign in again with the server HTTP token.';
      if (err.status === 429) {
        return err.retryAfter
          ? `Wait ${err.retryAfter} seconds before trying again.`
          : 'Wait a moment before trying again.';
      }
      if (err.status === 0) {
        return 'Could not reach the server. Check that aakb serve is running and try again.';
      }
      return 'Try the request again.';
  }
}

function renderErrorPanel(err, { heading = 'Request failed', onRetry = null } = {}) {
  const detailText =
    err instanceof ApiError
      ? err.detail || err.title || 'An unexpected error occurred.'
      : err && err.message
        ? err.message
        : 'Could not reach the server.';
  const guidance = nextStepForError(err);
  const codeBadge =
    err instanceof ApiError && err.code
      ? el('span', { className: 'badge badge-danger', textContent: err.code })
      : null;

  return el(
    'section',
    { className: 'panel panel-error', role: 'alert' },
    el(
      'div',
      { className: 'actions-row' },
      el('h2', { className: 'panel-title', textContent: heading }),
      codeBadge
    ),
    el('p', { textContent: detailText }),
    el('p', { textContent: guidance }),
    onRetry
      ? el(
          'div',
          { className: 'actions-row' },
          el('button', {
            type: 'button',
            className: 'btn btn-secondary btn-sm',
            textContent: 'Try again',
            onClick: onRetry,
          })
        )
      : null
  );
}

function handleUnauthorized(err) {
  if (activeController) {
    activeController.abort();
    activeController = null;
  }
  authToken = null;
  recordCache.clear();
  authMode = 'signin';
  signInMessage =
    err && err.detail
      ? `${err.detail} Sign in again to continue.`
      : 'Your session requires authentication. Sign in with the HTTP token.';
  updateShellVisibility();
  renderSignInView();
}

async function apiFetch(path, { method = 'GET', json = null, signal = null, tokenOverride = undefined, allowUnauthorized = false } = {}) {
  const headers = {
    Accept: 'application/json, application/problem+json',
  };
  const bearer = tokenOverride !== undefined ? tokenOverride : authToken;
  if (bearer) {
    headers.Authorization = `Bearer ${bearer}`;
  }
  const init = {
    method,
    headers,
    credentials: 'same-origin',
    signal,
  };
  if (json !== null) {
    headers['Content-Type'] = 'application/json';
    init.body = JSON.stringify(json);
  }

  let response;
  try {
    response = await fetch(path, init);
  } catch (networkErr) {
    if (networkErr && networkErr.name === 'AbortError') {
      throw networkErr;
    }
    throw new ApiError({
      status: 0,
      code: '',
      title: 'Network error',
      detail: 'Unable to connect to the aakb server.',
    });
  }

  if (!response.ok) {
    const problem = await parseProblem(response);
    if (response.status === 401 && !allowUnauthorized) {
      handleUnauthorized(problem);
    }
    throw problem;
  }

  const data = await response.json();
  return { status: response.status, headers: response.headers, data };
}

// ---------------------------------------------------------------------------
// Shell chrome & routing
// ---------------------------------------------------------------------------

function updateShellVisibility() {
  const authenticated = authMode === 'tokenless' || authMode === 'token';
  headerEl.hidden = !authenticated;
  footerEl.hidden = !authenticated;
  signOutBtn.hidden = authMode !== 'token';

  if (capabilities && capabilities.version) {
    versionEl.textContent = capabilities.version;
    versionEl.hidden = false;
  } else {
    versionEl.hidden = true;
  }
}

function updateActiveNav(routeName) {
  if (routeName === 'search') {
    navSearchEl.setAttribute('aria-current', 'page');
  } else {
    navSearchEl.removeAttribute('aria-current');
  }

  if (routeName === 'account') {
    navAccountEl.setAttribute('aria-current', 'page');
  } else {
    navAccountEl.removeAttribute('aria-current');
  }
}

function parseHash() {
  const raw = window.location.hash || '#/';
  const withoutHash = raw.startsWith('#') ? raw.slice(1) : raw;
  const qIndex = withoutHash.indexOf('?');
  const pathPart = qIndex >= 0 ? withoutHash.slice(0, qIndex) : withoutHash;
  const queryPart = qIndex >= 0 ? withoutHash.slice(qIndex + 1) : '';
  const path = pathPart || '/';
  const params = new URLSearchParams(queryPart);
  return { path, params, raw };
}

function routeCurrentHash() {
  if (authMode !== 'tokenless' && authMode !== 'token') {
    return;
  }

  if (activeController) {
    activeController.abort();
  }
  activeController = new AbortController();
  const { signal } = activeController;

  const { path, params, raw } = parseHash();

  if (path === '/' || path === '/search') {
    lastSearchHash = raw || '#/';
    updateActiveNav('search');
    renderSearchView(params, signal);
    return;
  }

  if (path === '/record') {
    lastRecordHash = raw;
    updateActiveNav('');
    renderRecordView(params.get('id') || '', signal);
    return;
  }

  if (path === '/read') {
    updateActiveNav('');
    renderReadView(params.get('id') || '', signal);
    return;
  }

  if (path === '/account') {
    updateActiveNav('account');
    renderAccountView(signal);
    return;
  }

  updateActiveNav('');
  renderNotFoundView(path);
}

// ---------------------------------------------------------------------------
// Auth & startup views
// ---------------------------------------------------------------------------

function renderAuthBrand() {
  return el(
    'div',
    { className: 'auth-brand' },
    el('img', { src: '/ui/icon.svg', alt: '', width: 36, height: 36 }),
    el('span', { className: 'auth-brand-name', textContent: 'aakb' })
  );
}

function renderLoadingPanel(message) {
  return el(
    'div',
    { className: 'panel loading-state', 'aria-busy': 'true' },
    el('span', { className: 'spinner', 'aria-hidden': 'true' }),
    el('span', { textContent: message })
  );
}

function renderMissingIdView(title, message) {
  clear(appEl);
  const heading = el('h1', { className: 'page-title', textContent: title });
  appEl.appendChild(
    el(
      'div',
      { className: 'view-stack' },
      heading,
      el(
        'section',
        { className: 'panel empty-state' },
        el('p', { textContent: message }),
        el('a', { href: '#/', className: 'btn btn-primary btn-sm', textContent: 'Back to Search' })
      )
    )
  );
  focusHeading(heading, title);
}

function renderBadgeGroup(title, items) {
  if (!Array.isArray(items) || items.length === 0) return null;
  return el(
    'div',
    null,
    el('h2', { className: 'section-title', textContent: title }),
    el(
      'div',
      { className: 'badge-row' },
      items.map((item) => el('span', { className: 'badge', textContent: item }))
    )
  );
}

function renderSignInView() {
  clear(appEl);

  const heading = el('h1', { className: 'page-title', textContent: 'Sign in' });
  const errorBox = el('div', { id: 'signin-error', role: 'alert', hidden: !signInMessage });
  if (signInMessage) {
    errorBox.className = 'panel panel-error';
    errorBox.textContent = signInMessage;
  }

  const tokenInput = el('input', {
    id: 'token-input',
    name: 'token',
    type: 'password',
    autocomplete: 'off',
    spellcheck: 'false',
    required: true,
  });

  const submitBtn = el('button', {
    type: 'submit',
    className: 'btn btn-primary',
    textContent: 'Sign in',
  });

  const form = el(
    'form',
    {
      className: 'auth-form',
      onSubmit: async (event) => {
        event.preventDefault();
        const candidate = tokenInput.value.trim();
        if (!candidate) {
          errorBox.className = 'panel panel-error';
          errorBox.textContent = 'Enter the HTTP token to sign in.';
          errorBox.hidden = false;
          tokenInput.focus();
          return;
        }

        submitBtn.disabled = true;
        submitBtn.textContent = 'Signing in…';
        errorBox.hidden = true;

        try {
          const { data } = await apiFetch('/api/v1/capabilities', {
            tokenOverride: candidate,
            allowUnauthorized: true,
          });
          authToken = candidate;
          tokenInput.value = '';
          capabilities = data || { version: '', openalex: false, auth_required: true };
          authMode = 'token';
          signInMessage = null;
          updateShellVisibility();
          announce('Signed in.');
          routeCurrentHash();
        } catch (err) {
          authToken = null;
          if (err instanceof ApiError && err.status === 403 && err.code === 'TOKEN_REQUIRED') {
            authMode = 'token_required';
            updateShellVisibility();
            renderTokenRequiredView(err);
            return;
          }
          const detail =
            err instanceof ApiError && err.status === 401
              ? err.detail || 'Invalid HTTP token. Check AAKB_HTTP_TOKEN and try again.'
              : `${err.detail || err.message || 'Sign-in failed.'} ${nextStepForError(err)}`;
          errorBox.className = 'panel panel-error';
          errorBox.textContent = detail;
          errorBox.hidden = false;
          submitBtn.disabled = false;
          submitBtn.textContent = 'Sign in';
          tokenInput.focus();
          tokenInput.select();
        }
      },
    },
    errorBox,
    el(
      'div',
      { className: 'field' },
      el('label', { for: 'token-input', textContent: 'HTTP token' }),
      tokenInput,
      el('span', {
        className: 'field-hint',
        textContent: 'Kept only in this tab until you reload or sign out.',
      })
    ),
    submitBtn
  );

  const shell = el(
    'div',
    { className: 'auth-shell' },
    renderAuthBrand(),
    el(
      'section',
      { className: 'panel' },
      heading,
      el('p', {
        className: 'muted',
        textContent: 'Enter the server AAKB_HTTP_TOKEN to search, read, and download.',
      }),
      form
    )
  );

  appEl.appendChild(shell);
  focusHeading(heading, 'Sign in');
}

function renderTokenRequiredView(err) {
  clear(appEl);

  const heading = el('h1', {
    className: 'page-title',
    textContent: 'HTTP token required',
  });

  const detail =
    err && err.detail
      ? err.detail
      : 'The server is listening on a non-loopback address without AAKB_HTTP_TOKEN configured.';

  const shell = el(
    'div',
    { className: 'auth-shell' },
    renderAuthBrand(),
    el(
      'section',
      { className: 'panel panel-warning', role: 'alert' },
      heading,
      el('p', { textContent: detail }),
      el('p', {
        textContent:
          'For safety, the REST API and web UI stay disabled on non-loopback addresses until AAKB_HTTP_TOKEN is set. Restart aakb serve with AAKB_HTTP_TOKEN configured, or bind it to 127.0.0.1 for local tokenless use.',
      }),
      el(
        'div',
        { className: 'actions-row' },
        el('button', {
          type: 'button',
          className: 'btn btn-secondary btn-sm',
          textContent: 'Retry connection',
          onClick: () => boot(),
        })
      )
    )
  );

  appEl.appendChild(shell);
  focusHeading(heading, 'HTTP token required');
}

function renderStartupErrorView(err) {
  clear(appEl);

  const heading = el('h1', { className: 'page-title', textContent: 'Unable to connect' });

  const shell = el(
    'div',
    { className: 'auth-shell' },
    renderAuthBrand(),
    heading,
    renderErrorPanel(err, {
      heading: err instanceof ApiError && err.title ? err.title : 'Server check failed',
      onRetry: () => boot(),
    })
  );

  appEl.appendChild(shell);
  focusHeading(heading, 'Unable to connect');
}

function renderNotFoundView(path) {
  clear(appEl);

  const heading = el('h1', { className: 'page-title', textContent: 'Page not found' });
  appEl.appendChild(
    el(
      'div',
      { className: 'view-stack' },
      heading,
      el(
        'section',
        { className: 'panel empty-state' },
        el('p', { textContent: `No view matches "${path}".` }),
        el(
          'div',
          { className: 'actions-row' },
          el('a', { href: '#/', className: 'btn btn-primary btn-sm', textContent: 'Go to Search' })
        )
      )
    )
  );
  focusHeading(heading, 'Page not found');
}

// ---------------------------------------------------------------------------
// Search view (#/ and #/search?...)
// ---------------------------------------------------------------------------

const SORT_OPTIONS = [
  { value: '', label: 'Relevance (default)' },
  { value: 'newest', label: 'Newest' },
  { value: 'oldest', label: 'Oldest' },
  { value: 'largest', label: 'Largest' },
  { value: 'smallest', label: 'Smallest' },
  { value: 'newest_added', label: 'Newest added' },
  { value: 'oldest_added', label: 'Oldest added' },
  { value: 'random', label: 'Random' },
];

function hasSearchCriteria(params) {
  return Boolean(
    (params.get('q') || '').trim() ||
      (params.get('title') || '').trim() ||
      (params.get('author') || '').trim() ||
      (params.get('publisher') || '').trim() ||
      (params.get('year') || '').trim()
  );
}

function hasAdvancedFilters(params) {
  return Boolean(
    params.get('title') ||
      params.get('author') ||
      params.get('publisher') ||
      params.get('year') ||
      params.get('format') ||
      params.get('language') ||
      params.get('sort') ||
      params.get('downloadable_only') === 'true' ||
      params.get('downloadable_only') === '1' ||
      params.get('stats') === 'true' ||
      params.get('stats') === '1'
  );
}

function buildSearchHash(params) {
  const clean = new URLSearchParams();
  for (const [k, v] of params.entries()) {
    if (!SEARCH_ALLOWED_PARAMS.has(k)) continue;
    const trimmed = v.trim();
    if (!trimmed) continue;
    if (k === 'page' && trimmed === '1') continue;
    if (k === 'limit' && trimmed === '10') continue;
    clean.set(k, trimmed);
  }
  const qs = clean.toString();
  return qs ? `#/search?${qs}` : '#/';
}

function buildSearchApiQuery(params) {
  const apiParams = new URLSearchParams();
  for (const [k, v] of params.entries()) {
    if (!SEARCH_ALLOWED_PARAMS.has(k)) continue;
    const trimmed = v.trim();
    if (!trimmed) continue;
    if (k === 'kind' && trimmed === 'openalex' && !capabilities.openalex) continue;
    apiParams.set(k, trimmed);
  }
  return apiParams.toString();
}

function renderRecordCard(rec, searchKind) {
  const id = recordIdentifier(rec);
  const isOA = searchKind === 'openalex' || isOpenAlexId(id);
  const targetHash = id ? `#/record?id=${encodeURIComponent(id)}` : null;

  if (id) {
    recordCache.set(id, rec);
  }

  const titleNode = targetHash
    ? el('a', {
        href: targetHash,
        textContent: rec.title || id || 'Untitled record',
        onClick: () => {
          if (id) recordCache.set(id, rec);
        },
      })
    : el('span', { textContent: rec.title || 'Untitled record' });

  const bylineParts = [];
  if (rec.authors) bylineParts.push(rec.authors);
  if (rec.publisher) bylineParts.push(rec.publisher);
  if (rec.edition) bylineParts.push(rec.edition);
  if (rec.year) bylineParts.push(rec.year);

  const badges = [];
  if (rec.format) badges.push(el('span', { className: 'badge badge-accent', textContent: rec.format.toUpperCase() }));
  if (rec.size) badges.push(el('span', { className: 'badge', textContent: rec.size }));
  if (rec.language) badges.push(el('span', { className: 'badge', textContent: rec.language }));
  if (isOA) {
    badges.push(el('span', { className: 'badge badge-accent', textContent: 'OpenAlex' }));
  } else if (rec.kind) {
    badges.push(el('span', { className: 'badge', textContent: rec.kind }));
  }
  if (rec.fast_download) {
    badges.push(el('span', { className: 'badge badge-success', textContent: 'Fast download' }));
  }
  if (rec.possible_issues) {
    badges.push(el('span', { className: 'badge badge-warning', textContent: 'Possible issues' }));
  }
  if (rec.doi) {
    badges.push(el('span', { className: 'badge', textContent: `DOI: ${rec.doi}` }));
  }

  let statsRow = null;
  if (rec.stats) {
    const s = rec.stats;
    statsRow = el(
      'div',
      { className: 'result-stats' },
      el('span', { textContent: `Downloads: ${s.downloads ?? 0}` }),
      el('span', { textContent: `Great quality: ${s.great_quality ?? 0}` }),
      el('span', { textContent: `Reports: ${s.reports ?? 0}` }),
      el('span', { textContent: `Lists: ${s.lists ?? 0}` }),
      el('span', { textContent: `Comments: ${s.comments ?? 0}` })
    );
  }

  const actions = [];
  if (targetHash) {
    actions.push(
      el('a', {
        href: targetHash,
        className: 'btn btn-secondary btn-sm',
        textContent: 'View record',
        onClick: () => {
          if (id) recordCache.set(id, rec);
        },
      })
    );
  }
  const readTarget = isOA ? id : capabilities.openalex && rec.doi ? rec.doi : null;
  if (readTarget && capabilities.openalex) {
    actions.push(
      el('a', {
        href: `#/read?id=${encodeURIComponent(readTarget)}`,
        className: 'btn btn-ghost btn-sm',
        textContent: 'Read',
      })
    );
  }

  return el(
    'li',
    { className: 'result-card' },
    el('h3', { className: 'result-title' }, titleNode),
    bylineParts.length > 0 ? el('p', { className: 'result-byline', textContent: bylineParts.join(' — ') }) : null,
    badges.length > 0 ? el('div', { className: 'result-meta' }, badges) : null,
    statsRow,
    actions.length > 0 ? el('div', { className: 'result-actions' }, actions) : null
  );
}

function renderSearchView(params, signal) {
  clear(appEl);

  const heading = el('h1', { className: 'page-title', textContent: 'Search' });

  const qInput = el('input', {
    id: 'search-q',
    name: 'q',
    type: 'search',
    value: params.get('q') || '',
    placeholder: 'Title, author, ISBN, or DOI',
    autocomplete: 'off',
  });

  const kindOptions = [
    { value: '', label: 'Auto' },
    { value: 'book', label: 'Books' },
    { value: 'article', label: 'Articles' },
    { value: 'metadata', label: 'Metadata' },
  ];
  if (capabilities.openalex) {
    kindOptions.push({ value: 'openalex', label: 'OpenAlex' });
  }

  const currentKind = params.get('kind') || '';
  const kindSelect = el(
    'select',
    { id: 'search-kind', name: 'kind' },
    kindOptions.map((opt) =>
      el('option', {
        value: opt.value,
        selected: opt.value === currentKind,
        textContent: opt.label,
      })
    )
  );

  const titleInput = el('input', {
    id: 'filter-title',
    name: 'title',
    type: 'text',
    value: params.get('title') || '',
  });
  const authorInput = el('input', {
    id: 'filter-author',
    name: 'author',
    type: 'text',
    value: params.get('author') || '',
  });
  const publisherInput = el('input', {
    id: 'filter-publisher',
    name: 'publisher',
    type: 'text',
    value: params.get('publisher') || '',
  });
  const yearInput = el('input', {
    id: 'filter-year',
    name: 'year',
    type: 'text',
    value: params.get('year') || '',
    placeholder: 'e.g. 2020',
  });
  const formatInput = el('input', {
    id: 'filter-format',
    name: 'format',
    type: 'text',
    value: params.get('format') || '',
    placeholder: 'e.g. epub, pdf, -mobi',
  });
  const languageInput = el('input', {
    id: 'filter-language',
    name: 'language',
    type: 'text',
    value: params.get('language') || '',
    placeholder: 'e.g. en, de',
  });

  const currentSort = params.get('sort') || '';
  const sortSelect = el(
    'select',
    { id: 'filter-sort', name: 'sort' },
    SORT_OPTIONS.map((opt) =>
      el('option', {
        value: opt.value,
        selected: opt.value === currentSort,
        textContent: opt.label,
      })
    )
  );

  const downloadableCheckbox = el('input', {
    id: 'filter-downloadable',
    name: 'downloadable_only',
    type: 'checkbox',
    checked: params.get('downloadable_only') === 'true' || params.get('downloadable_only') === '1',
  });

  const statsCheckbox = el('input', {
    id: 'filter-stats',
    name: 'stats',
    type: 'checkbox',
    checked: params.get('stats') === 'true' || params.get('stats') === '1',
  });

  const validationEl = el('div', { role: 'alert', hidden: true });
  const resultsContainer = el('div', { className: 'view-stack', 'aria-live': 'polite' });

  const submitBtn = el('button', {
    type: 'submit',
    className: 'btn btn-primary',
    textContent: 'Search',
  });

  const clearBtn = el('button', {
    type: 'button',
    className: 'btn btn-ghost btn-sm',
    textContent: 'Clear filters',
    onClick: () => {
      titleInput.value = '';
      authorInput.value = '';
      publisherInput.value = '';
      yearInput.value = '';
      formatInput.value = '';
      languageInput.value = '';
      sortSelect.value = '';
      downloadableCheckbox.checked = false;
      statsCheckbox.checked = false;
    },
  });

  const filtersDetails = el(
    'details',
    { className: 'filters-details', open: hasAdvancedFilters(params) },
    el('summary', { textContent: 'Advanced filters' }),
    el(
      'div',
      { className: 'filters-grid' },
      el('div', { className: 'field' }, el('label', { for: 'filter-title', textContent: 'Title' }), titleInput),
      el('div', { className: 'field' }, el('label', { for: 'filter-author', textContent: 'Author' }), authorInput),
      el('div', { className: 'field' }, el('label', { for: 'filter-publisher', textContent: 'Publisher' }), publisherInput),
      el('div', { className: 'field' }, el('label', { for: 'filter-year', textContent: 'Year' }), yearInput),
      el('div', { className: 'field' }, el('label', { for: 'filter-format', textContent: 'Format' }), formatInput),
      el('div', { className: 'field' }, el('label', { for: 'filter-language', textContent: 'Language' }), languageInput),
      el('div', { className: 'field' }, el('label', { for: 'filter-sort', textContent: 'Sort' }), sortSelect)
    ),
    el(
      'div',
      { className: 'filters-toggles' },
      el(
        'div',
        { className: 'filters-checkboxes' },
        el('label', { className: 'checkbox-field', for: 'filter-downloadable' }, downloadableCheckbox, 'Downloadable only'),
        el('label', { className: 'checkbox-field', for: 'filter-stats' }, statsCheckbox, 'Include stats')
      ),
      clearBtn
    )
  );

  const form = el(
    'form',
    {
      className: 'panel search-form',
      onSubmit: (event) => {
        event.preventDefault();
        const nextParams = new URLSearchParams();
        // Preserve supported extra parameters (such as source, content, work_type, limit, rank, search_descriptions)
        for (const extra of ['work_type', 'source', 'content', 'limit', 'rank', 'search_descriptions', 'timeout_seconds']) {
          const val = params.get(extra);
          if (val) nextParams.set(extra, val);
        }

        if (qInput.value.trim()) nextParams.set('q', qInput.value.trim());
        if (kindSelect.value) nextParams.set('kind', kindSelect.value);
        if (titleInput.value.trim()) nextParams.set('title', titleInput.value.trim());
        if (authorInput.value.trim()) nextParams.set('author', authorInput.value.trim());
        if (publisherInput.value.trim()) nextParams.set('publisher', publisherInput.value.trim());
        if (yearInput.value.trim()) nextParams.set('year', yearInput.value.trim());
        if (formatInput.value.trim()) nextParams.set('format', formatInput.value.trim());
        if (languageInput.value.trim()) nextParams.set('language', languageInput.value.trim());
        if (sortSelect.value) nextParams.set('sort', sortSelect.value);
        if (downloadableCheckbox.checked) nextParams.set('downloadable_only', 'true');
        if (statsCheckbox.checked) nextParams.set('stats', 'true');

        if (!hasSearchCriteria(nextParams)) {
          validationEl.className = 'panel panel-warning';
          validationEl.textContent = 'Enter a search query or at least one of Title, Author, Publisher, or Year.';
          validationEl.hidden = false;
          qInput.focus();
          return;
        }
        validationEl.hidden = true;

        const nextHash = buildSearchHash(nextParams);
        if (window.location.hash === nextHash) {
          routeCurrentHash();
        } else {
          window.location.hash = nextHash;
        }
      },
    },
    el(
      'div',
      { className: 'search-primary' },
      el('div', { className: 'field' }, el('label', { for: 'search-q', textContent: 'Query' }), qInput),
      el('div', { className: 'field' }, el('label', { for: 'search-kind', textContent: 'Kind' }), kindSelect),
      submitBtn
    ),
    validationEl,
    filtersDetails
  );

  appEl.appendChild(el('div', { className: 'view-stack' }, heading, form, resultsContainer));

  const queryTitle = (params.get('q') || params.get('title') || params.get('author') || '').trim();
  focusHeading(heading, queryTitle ? `Search: ${queryTitle}` : 'Search');

  if (!hasSearchCriteria(params)) {
    const scopeText = capabilities.openalex
      ? 'Search books, articles, DOIs, library catalog metadata, or OpenAlex papers.'
      : 'Search books, articles, DOIs, or library catalog metadata.';
    resultsContainer.appendChild(
      el(
        'section',
        { className: 'panel empty-state' },
        el('h2', { textContent: 'Start a search' }),
        el('p', { textContent: scopeText })
      )
    );
    return;
  }

  runSearch(params, resultsContainer, submitBtn, signal);
}

async function runSearch(params, container, submitBtn, signal) {
  clear(container);
  submitBtn.disabled = true;
  container.setAttribute('aria-busy', 'true');
  container.appendChild(
    el(
      'div',
      { className: 'panel loading-state' },
      el('span', { className: 'spinner', 'aria-hidden': 'true' }),
      el('span', { textContent: 'Searching…' })
    )
  );

  const page = Math.max(1, Number.parseInt(params.get('page') || '1', 10) || 1);
  const qs = buildSearchApiQuery(params);

  try {
    const { data } = await apiFetch(`/api/v1/search?${qs}`, { signal });
    if (signal.aborted) return;

    clear(container);
    container.removeAttribute('aria-busy');
    submitBtn.disabled = false;

    const results = Array.isArray(data.results) ? data.results : [];
    const partial = Array.isArray(data.partial_matches) ? data.partial_matches : [];
    const total = typeof data.total === 'number' ? data.total : results.length;
    const totalText = data.total_is_lower_bound ? `${total.toLocaleString()}+` : total.toLocaleString();

    if (results.length === 0 && partial.length === 0) {
      container.appendChild(
        el(
          'section',
          { className: 'panel empty-state' },
          el('h2', { textContent: 'No results found' }),
          el('p', {
            textContent:
              'Try broader keywords, switch Kind to Auto, or clear one of the advanced filters.',
          })
        )
      );
      announce('No results found.');
      return;
    }

    const summaryText = `Showing ${results.length} result${results.length === 1 ? '' : 's'} of ${totalText} (page ${page})`;
    container.appendChild(
      el(
        'div',
        { className: 'results-summary' },
        el('span', { textContent: summaryText }),
        data.kind ? el('span', { className: 'badge', textContent: `Kind: ${data.kind}` }) : null
      )
    );

    if (results.length > 0) {
      container.appendChild(
        el(
          'section',
          { 'aria-labelledby': 'ranked-results-heading' },
          el('h2', { id: 'ranked-results-heading', className: 'section-title', textContent: 'Results' }),
          el(
            'ol',
            { className: 'result-list' },
            results.map((rec) => renderRecordCard(rec, data.kind))
          )
        )
      );
    }

    if (partial.length > 0) {
      container.appendChild(
        el(
          'section',
          { className: 'looser-section', 'aria-labelledby': 'looser-matches-heading' },
          el('h2', { id: 'looser-matches-heading', className: 'section-title', textContent: 'Looser matches' }),
          el(
            'ol',
            { className: 'result-list' },
            partial.map((rec) => renderRecordCard(rec, data.kind))
          )
        )
      );
    }

    const hasPrev = page > 1;
    const hasNext = Boolean(data.more_pages && data.next_page);
    if (hasPrev || hasNext) {
      const prevParams = new URLSearchParams(params);
      prevParams.set('page', String(page - 1));

      const nextParams = new URLSearchParams(params);
      if (hasNext) nextParams.set('page', String(data.next_page));

      container.appendChild(
        el(
          'nav',
          { className: 'pagination', 'aria-label': 'Pagination' },
          hasPrev
            ? el('a', {
                href: buildSearchHash(prevParams),
                className: 'btn btn-secondary btn-sm',
                textContent: 'Previous page',
              })
            : el('span'),
          el('span', { className: 'pagination-info', textContent: `Page ${page}` }),
          hasNext
            ? el('a', {
                href: buildSearchHash(nextParams),
                className: 'btn btn-secondary btn-sm',
                textContent: 'Next page',
              })
            : el('span')
        )
      );
    }

    announce(summaryText);
  } catch (err) {
    if (err && err.name === 'AbortError') return;
    if (authMode === 'signin') return;

    clear(container);
    container.removeAttribute('aria-busy');
    submitBtn.disabled = false;
    container.appendChild(
      renderErrorPanel(err, {
        heading: 'Search failed',
        onRetry: () => runSearch(params, container, submitBtn, signal),
      })
    );
  }
}

// ---------------------------------------------------------------------------
// Download panel (embedded inside Record view)
// ---------------------------------------------------------------------------

function createDownloadPanel(target, signal) {
  const panel = el('section', {
    className: 'download-panel',
    'aria-live': 'polite',
    'aria-label': 'Download status',
    hidden: true,
  });

  async function saveWithToken(res, saveBtn, feedbackEl) {
    const fileUrl = safeDownloadUrl(res.file_url);
    if (!fileUrl) {
      feedbackEl.textContent = 'Invalid download URL returned by server.';
      return;
    }

    const bytes = typeof res.bytes === 'number' ? res.bytes : 0;

    if (bytes > MAX_BLOB_BYTES && typeof window.showSaveFilePicker === 'function') {
      let fileHandle;
      try {
        fileHandle = await window.showSaveFilePicker({ suggestedName: res.name || 'download' });
      } catch (err) {
        if (err && err.name === 'AbortError') {
          feedbackEl.textContent = 'Save canceled.';
          return;
        }
        feedbackEl.textContent = `Could not open save dialog: ${err.message || err}`;
        return;
      }

      saveBtn.disabled = true;
      saveBtn.textContent = 'Saving…';
      feedbackEl.textContent = 'Streaming file to disk…';

      try {
        const resp = await fetch(fileUrl, {
          headers: authToken ? { Authorization: `Bearer ${authToken}` } : {},
          credentials: 'same-origin',
          signal,
        });
        if (!resp.ok) {
          const problem = await parseProblem(resp);
          if (resp.status === 401) {
            handleUnauthorized(problem);
            return;
          }
          throw problem;
        }
        if (!resp.body) {
          throw new Error('Browser response stream unavailable.');
        }
        const writable = await fileHandle.createWritable();
        await resp.body.pipeTo(writable);
        feedbackEl.textContent = 'File saved.';
        announce('File saved.');
      } catch (err) {
        if (err && err.name === 'AbortError') return;
        feedbackEl.textContent = `Save failed: ${err.detail || err.message || 'Unknown error'}`;
      } finally {
        saveBtn.disabled = false;
        saveBtn.textContent = 'Save file';
      }
      return;
    }

    saveBtn.disabled = true;
    saveBtn.textContent = 'Saving…';
    feedbackEl.textContent = 'Fetching verified file…';

    try {
      const resp = await fetch(fileUrl, {
        headers: authToken ? { Authorization: `Bearer ${authToken}` } : {},
        credentials: 'same-origin',
        signal,
      });
      if (!resp.ok) {
        const problem = await parseProblem(resp);
        if (resp.status === 401) {
          handleUnauthorized(problem);
          return;
        }
        throw problem;
      }
      const blob = await resp.blob();
      const objectUrl = URL.createObjectURL(blob);
      const link = el('a', {
        href: objectUrl,
        download: res.name || 'download',
        hidden: true,
      });
      document.body.appendChild(link);
      link.click();
      document.body.removeChild(link);
      window.setTimeout(() => URL.revokeObjectURL(objectUrl), 1000);
      feedbackEl.textContent = 'File download started.';
      announce('File download started.');
    } catch (err) {
      if (err && err.name === 'AbortError') return;
      feedbackEl.textContent = `Save failed: ${err.detail || err.message || 'Unknown error'}`;
    } finally {
      saveBtn.disabled = false;
      saveBtn.textContent = 'Save file';
    }
  }

  function renderCliFallback() {
    const cmd = buildDownloadCliCommand(target);
    const copyFeedback = el('span', { className: 'field-hint' });
    const copyBtn = el('button', {
      type: 'button',
      className: 'btn btn-secondary btn-sm',
      textContent: 'Copy command',
      onClick: async () => {
        try {
          if (navigator.clipboard && typeof navigator.clipboard.writeText === 'function') {
            await navigator.clipboard.writeText(cmd);
            copyFeedback.textContent = 'Copied.';
            announce('CLI command copied.');
          } else {
            copyFeedback.textContent = 'Copy manually from the command box.';
          }
        } catch {
          copyFeedback.textContent = 'Copy manually from the command box.';
        }
      },
    });

    return el(
      'div',
      { className: 'view-stack' },
      el('p', {
        textContent:
          'Files over 64 MiB in token mode need Chrome File System Access or the CLI. Run this command to download directly:',
      }),
      el('div', { className: 'cli-box' }, el('code', { textContent: cmd }), copyBtn),
      copyFeedback
    );
  }

  async function start(triggerBtn) {
    panel.hidden = false;
    panel.className = 'download-panel';
    clear(panel);
    panel.appendChild(
      el(
        'div',
        { className: 'loading-state' },
        el('span', { className: 'spinner', 'aria-hidden': 'true' }),
        el('span', { textContent: 'Preparing download…' })
      )
    );
    if (triggerBtn) triggerBtn.disabled = true;

    const payload = {};
    if (target.hash) payload.hash = target.hash;
    else if (target.doi) payload.doi = target.doi;
    else if (target.openalex_id) payload.openalex_id = target.openalex_id;
    if (target.title) payload.title = target.title;
    if (target.format) payload.format = target.format;

    try {
      const { data } = await apiFetch('/api/v1/downloads', {
        method: 'POST',
        json: payload,
        signal,
      });
      if (signal.aborted) return;

      panel.className = 'download-panel download-panel-ready';
      clear(panel);

      const readyLabel = data.reused ? 'Ready (reused existing file)' : 'Ready (saved new file)';
      const digestValue =
        data.verification === 'md5' && data.md5
          ? `MD5: ${data.md5}`
          : data.verification === 'sha256' && data.sha256
            ? `SHA-256: ${data.sha256}`
            : data.verification || 'none';

      const detailItems = [
        el('div', null, el('dt', { textContent: 'File name' }), el('dd', { textContent: data.name || '' })),
        el('div', null, el('dt', { textContent: 'Size' }), el('dd', { textContent: formatBytes(data.bytes) })),
        el('div', null, el('dt', { textContent: 'Verification' }), el('dd', null, el('code', { textContent: digestValue }))),
      ];
      if (typeof data.downloads_left === 'number') {
        detailItems.push(
          el(
            'div',
            null,
            el('dt', { textContent: 'Fast downloads left' }),
            el('dd', { textContent: String(data.downloads_left) })
          )
        );
      }

      const fileUrl = safeDownloadUrl(data.file_url);
      let saveControl;
      if (authMode === 'tokenless' && fileUrl) {
        saveControl = el(
          'div',
          { className: 'actions-row' },
          el('a', {
            href: fileUrl,
            download: data.name || '',
            className: 'btn btn-primary',
            textContent: 'Save file',
          })
        );
      } else if (
        authMode === 'token' &&
        typeof data.bytes === 'number' &&
        data.bytes > MAX_BLOB_BYTES &&
        typeof window.showSaveFilePicker !== 'function'
      ) {
        saveControl = renderCliFallback();
      } else {
        const feedbackEl = el('span', { className: 'field-hint' });
        const saveBtn = el('button', {
          type: 'button',
          className: 'btn btn-primary',
          textContent: 'Save file',
        });
        saveBtn.addEventListener('click', () => saveWithToken(data, saveBtn, feedbackEl));
        saveControl = el('div', { className: 'actions-row' }, saveBtn, feedbackEl);
      }

      panel.appendChild(
        el(
          'div',
          { className: 'download-status-row' },
          el('span', { className: 'badge badge-success', textContent: readyLabel })
        )
      );
      panel.appendChild(el('dl', { className: 'download-details' }, detailItems));
      panel.appendChild(saveControl);

      if (triggerBtn) triggerBtn.disabled = false;
    } catch (err) {
      if (err && err.name === 'AbortError') return;
      if (authMode === 'signin') return;

      panel.className = 'download-panel download-panel-failed';
      clear(panel);

      const detail = err instanceof ApiError ? err.detail || err.title : err.message || 'Download failed.';
      panel.appendChild(
        el(
          'div',
          { className: 'download-status-row' },
          el('span', { className: 'badge badge-danger', textContent: 'Download failed' }),
          err instanceof ApiError && err.code
            ? el('span', { className: 'badge badge-danger', textContent: err.code })
            : null
        )
      );
      panel.appendChild(el('p', { textContent: detail }));
      panel.appendChild(el('p', { className: 'muted', textContent: nextStepForError(err) }));
      panel.appendChild(
        el(
          'div',
          { className: 'actions-row' },
          el('button', {
            type: 'button',
            className: 'btn btn-secondary btn-sm',
            textContent: 'Try download again',
            onClick: () => start(triggerBtn),
          })
        )
      );
      if (triggerBtn) triggerBtn.disabled = false;
    }
  }

  return { panel, start };
}

// ---------------------------------------------------------------------------
// Record view (#/record?id=...)
// ---------------------------------------------------------------------------

function addMetaItem(list, label, value) {
  if (value === undefined || value === null || value === '') return;
  list.push(
    el(
      'div',
      { className: 'meta-item' },
      el('dt', { textContent: label }),
      el('dd', null, typeof value === 'string' || typeof value === 'number' ? String(value) : value)
    )
  );
}

function renderOpenAlexRecordView(id, signal) {
  clear(appEl);

  const cached = recordCache.get(id) || null;
  const title = (cached && cached.title) || `OpenAlex work ${id}`;
  const heading = el('h1', { className: 'page-title', textContent: title });

  const downloadTarget = {
    openalex_id: id,
    title: cached && cached.title ? cached.title : '',
    format: cached && cached.format ? cached.format : '',
  };
  const downloadUI = createDownloadPanel(downloadTarget, signal);

  const downloadBtn = el('button', {
    type: 'button',
    className: 'btn btn-primary',
    textContent: 'Download',
    onClick: () => downloadUI.start(downloadBtn),
  });

  const actionsBar = el(
    'div',
    { className: 'actions-row' },
    capabilities.openalex
      ? el('a', {
          href: `#/read?id=${encodeURIComponent(id)}`,
          className: 'btn btn-primary',
          textContent: 'Read article',
        })
      : null,
    downloadBtn
  );

  // Use secondary style for Download when Read is also primary
  if (capabilities.openalex) {
    downloadBtn.className = 'btn btn-secondary';
  }

  const metaItems = [];
  addMetaItem(metaItems, 'OpenAlex ID', el('code', { textContent: id }));
  if (cached) {
    if (cached.year) addMetaItem(metaItems, 'Year', cached.year);
    if (cached.publisher) addMetaItem(metaItems, 'Publisher / Venue', cached.publisher);
    if (cached.language) addMetaItem(metaItems, 'Language', cached.language);
    if (cached.format) addMetaItem(metaItems, 'Format', cached.format.toUpperCase());
    if (cached.size) addMetaItem(metaItems, 'Size', cached.size);
    if (cached.doi) addMetaItem(metaItems, 'DOI', el('code', { textContent: cached.doi }));
    const extUrl = safeExternalUrl(cached.url);
    if (extUrl) {
      addMetaItem(
        metaItems,
        'Source URL',
        el('a', { href: extUrl, target: '_blank', rel: 'noopener noreferrer', textContent: extUrl })
      );
    }
    if (Array.isArray(cached.sources) && cached.sources.length > 0) {
      addMetaItem(metaItems, 'Sources', cached.sources.join(', '));
    }
    addMetaItem(metaItems, 'Downloadable', cached.fast_download ? 'Yes' : 'Check via Download');
  }

  appEl.appendChild(
    el(
      'div',
      { className: 'view-stack' },
      el('a', { href: lastSearchHash || '#/', className: 'back-link', textContent: 'Back to Search' }),
      el(
        'article',
        { className: 'panel view-stack' },
        el(
          'div',
          { className: 'record-header' },
          el('div', { className: 'badge-row' }, el('span', { className: 'badge badge-accent', textContent: 'OpenAlex article' })),
          heading,
          cached && cached.authors ? el('p', { className: 'record-authors', textContent: cached.authors }) : null
        ),
        !cached
          ? el('p', {
              className: 'muted',
              textContent:
                'Opened directly by OpenAlex ID. Full metadata appears when opened from Search results; you can still read or download the work below.',
            })
          : null,
        actionsBar,
        downloadUI.panel,
        el('dl', { className: 'meta-grid' }, metaItems)
      )
    )
  );

  focusHeading(heading, title);
}

async function renderRecordView(id, signal) {
  const trimmedId = String(id || '').trim();
  if (!trimmedId) {
    renderMissingIdView('Record', 'No record identifier was provided.');
    return;
  }

  if (isOpenAlexId(trimmedId)) {
    renderOpenAlexRecordView(trimmedId, signal);
    return;
  }

  clear(appEl);
  const loadingHeading = el('h1', { className: 'page-title', textContent: 'Record' });
  appEl.appendChild(
    el(
      'div',
      { className: 'view-stack' },
      el('a', { href: lastSearchHash || '#/', className: 'back-link', textContent: 'Back to Search' }),
      loadingHeading,
      renderLoadingPanel('Loading record details…')
    )
  );
  focusHeading(loadingHeading, 'Record');

  try {
    const { data } = await apiFetch(`/api/v1/records/lookup?id=${encodeURIComponent(trimmedId)}`, { signal });
    if (signal.aborted) return;

    clear(appEl);

    const cached = recordCache.get(trimmedId) || null;
    const title = data.title || data.id || trimmedId;
    const heading = el('h1', { className: 'page-title', textContent: title });
    const doi = extractDOIFromDetails(data, cached);

    const downloadTarget = data.hash
      ? { hash: data.hash, title: data.title || '', format: data.format || '' }
      : doi
        ? { doi, title: data.title || '', format: data.format || '' }
        : null;

    const downloadUI = downloadTarget ? createDownloadPanel(downloadTarget, signal) : null;

    const actionButtons = [];
    if (downloadTarget && downloadUI) {
      const downloadBtn = el('button', {
        type: 'button',
        className: 'btn btn-primary',
        textContent: 'Download',
        onClick: () => downloadUI.start(downloadBtn),
      });
      actionButtons.push(downloadBtn);
    } else {
      actionButtons.push(
        el('span', {
          className: 'muted',
          textContent: 'No direct file hash or DOI is attached to this catalog record.',
        })
      );
    }

    if (doi && capabilities.openalex) {
      actionButtons.push(
        el('a', {
          href: `#/read?id=${encodeURIComponent(doi)}`,
          className: 'btn btn-secondary',
          textContent: 'Read article',
        })
      );
    }

    const badges = [];
    if (data.format) badges.push(el('span', { className: 'badge badge-accent', textContent: data.format.toUpperCase() }));
    if (typeof data.size_bytes === 'number' && data.size_bytes > 0) {
      badges.push(el('span', { className: 'badge', textContent: formatBytes(data.size_bytes) }));
    }
    if (data.fast_download) {
      badges.push(el('span', { className: 'badge badge-success', textContent: 'Fast download' }));
    }
    if (Array.isArray(data.problems) && data.problems.length > 0) {
      badges.push(
        el('span', {
          className: 'badge badge-warning',
          textContent: `${data.problems.length} reported problem${data.problems.length === 1 ? '' : 's'}`,
        })
      );
    }

    const metaItems = [];
    if (data.publisher) addMetaItem(metaItems, 'Publisher', data.publisher);
    if (data.edition) addMetaItem(metaItems, 'Edition', data.edition);
    if (data.year) addMetaItem(metaItems, 'Year', data.year);
    if (Array.isArray(data.languages) && data.languages.length > 0) {
      addMetaItem(metaItems, 'Languages', data.languages.join(', '));
    }
    if (data.format) addMetaItem(metaItems, 'Format', data.format.toUpperCase());
    if (typeof data.size_bytes === 'number' && data.size_bytes > 0) {
      addMetaItem(metaItems, 'File size', formatBytes(data.size_bytes));
    }
    addMetaItem(metaItems, 'Fast download', data.fast_download ? 'Yes' : 'No');
    if (data.hash) addMetaItem(metaItems, 'MD5 hash', el('code', { textContent: data.hash }));
    if (data.id && data.id !== data.hash) addMetaItem(metaItems, 'Record ID', el('code', { textContent: data.id }));
    const extUrl = safeExternalUrl(data.url);
    if (extUrl) {
      addMetaItem(
        metaItems,
        'Record URL',
        el('a', { href: extUrl, target: '_blank', rel: 'noopener noreferrer', textContent: extUrl })
      );
    }

    const sections = [
      el(
        'article',
        { className: 'panel view-stack' },
        el(
          'div',
          { className: 'record-header' },
          badges.length > 0 ? el('div', { className: 'badge-row' }, badges) : null,
          heading,
          data.authors ? el('p', { className: 'record-authors', textContent: data.authors }) : null
        ),
        el('div', { className: 'actions-row' }, actionButtons),
        downloadUI ? downloadUI.panel : null,
        el('dl', { className: 'meta-grid' }, metaItems)
      ),
    ];

    if (data.description) {
      const paragraphs = String(data.description)
        .split(/\n{2,}/)
        .map((p) => p.trim())
        .filter(Boolean);
      sections.push(
        el(
          'section',
          { className: 'panel' },
          el('h2', { className: 'section-title', textContent: 'Description' }),
          el(
            'div',
            { className: 'prose' },
            paragraphs.map((p) => el('p', { textContent: p }))
          )
        )
      );
    }

    if (Array.isArray(data.problems) && data.problems.length > 0) {
      sections.push(
        el(
          'section',
          { className: 'panel' },
          el('h2', { className: 'section-title', textContent: 'Reported problems' }),
          el(
            'ul',
            { className: 'problem-list' },
            data.problems.map((prob) =>
              el(
                'li',
                { className: 'problem-item' },
                el('span', { className: 'problem-type', textContent: prob.type || 'Issue' }),
                prob.description ? el('p', { className: 'problem-desc', textContent: prob.description }) : null,
                prob.better_hash
                  ? el(
                      'div',
                      null,
                      el('a', {
                        href: `#/record?id=${encodeURIComponent(prob.better_hash)}`,
                        textContent: `Open suggested better file (${prob.better_hash})`,
                      })
                    )
                  : null
              )
            )
          )
        )
      );
    }

    if (data.stats) {
      const s = data.stats;
      sections.push(
        el(
          'section',
          { className: 'panel' },
          el('h2', { className: 'section-title', textContent: 'Statistics' }),
          el(
            'div',
            { className: 'stats-grid' },
            [
              ['Downloads', s.downloads],
              ['Great quality', s.great_quality],
              ['Reports', s.reports],
              ['Lists', s.lists],
              ['Comments', s.comments],
            ].map(([label, val]) =>
              el(
                'div',
                { className: 'stat-card' },
                el('div', { className: 'stat-value', textContent: typeof val === 'number' ? val.toLocaleString() : '0' }),
                el('div', { className: 'stat-label', textContent: label })
              )
            )
          )
        )
      );
    }

    const idEntries =
      data.identifiers && typeof data.identifiers === 'object'
        ? Object.entries(data.identifiers).filter(([, vals]) => Array.isArray(vals) && vals.length > 0)
        : [];
    if (idEntries.length > 0) {
      sections.push(
        el(
          'section',
          { className: 'panel' },
          el('h2', { className: 'section-title', textContent: 'Identifiers' }),
          el(
            'dl',
            { className: 'meta-grid' },
            idEntries.map(([key, vals]) =>
              el(
                'div',
                { className: 'meta-item' },
                el('dt', { textContent: key }),
                el(
                  'dd',
                  null,
                  vals.map((v, idx) => [idx > 0 ? ', ' : null, el('code', { textContent: v })])
                )
              )
            )
          )
        )
      );
    }

    const subjectsGroup = renderBadgeGroup('Subjects', data.subjects);
    const sourcesGroup = renderBadgeGroup('Sources', data.sources);
    if (subjectsGroup || sourcesGroup) {
      sections.push(
        el(
          'section',
          { className: 'panel view-stack' },
          subjectsGroup,
          sourcesGroup
        )
      );
    }

    appEl.appendChild(
      el(
        'div',
        { className: 'view-stack' },
        el('a', { href: lastSearchHash || '#/', className: 'back-link', textContent: 'Back to Search' }),
        sections
      )
    );

    focusHeading(heading, title);
  } catch (err) {
    if (err && err.name === 'AbortError') return;
    if (authMode === 'signin') return;

    clear(appEl);
    const errHeading = el('h1', { className: 'page-title', textContent: 'Record unavailable' });
    appEl.appendChild(
      el(
        'div',
        { className: 'view-stack' },
        el('a', { href: lastSearchHash || '#/', className: 'back-link', textContent: 'Back to Search' }),
        errHeading,
        renderErrorPanel(err, {
          heading: 'Could not load record',
          onRetry: () => renderRecordView(trimmedId, signal),
        })
      )
    );
    focusHeading(errHeading, 'Record unavailable');
  }
}

// ---------------------------------------------------------------------------
// Read view (#/read?id=...)
// ---------------------------------------------------------------------------

async function renderReadView(id, signal) {
  const trimmedId = String(id || '').trim();
  const backHref = lastRecordHash || lastSearchHash || '#/';
  const backLabel = lastRecordHash ? 'Back to Record' : 'Back to Search';

  if (!trimmedId) {
    renderMissingIdView('Read article', 'No article identifier was provided.');
    return;
  }

  clear(appEl);
  const loadingHeading = el('h1', { className: 'page-title', textContent: 'Read article' });
  appEl.appendChild(
    el(
      'div',
      { className: 'view-stack' },
      el('a', { href: backHref, className: 'back-link', textContent: backLabel }),
      loadingHeading,
      renderLoadingPanel('Fetching article text…')
    )
  );
  focusHeading(loadingHeading, 'Read article');

  try {
    const { data } = await apiFetch('/api/v1/article-reads', {
      method: 'POST',
      json: { id: trimmedId },
      signal,
    });
    if (signal.aborted) return;

    clear(appEl);

    const title = data.title || trimmedId;
    const heading = el('h1', { className: 'page-title', textContent: title });
    const sourceInfo = formatSourceLabel(data.source);

    const seenWarnings = new Set();
    const warningsPanel = el('section', { className: 'panel panel-warning', hidden: true });
    const warningsList = el('ul', { className: 'warning-list' });
    warningsPanel.appendChild(el('h2', { className: 'panel-title', textContent: 'Warnings' }));
    warningsPanel.appendChild(warningsList);

    function appendWarnings(warnings) {
      if (!Array.isArray(warnings)) return;
      for (const w of warnings) {
        const text = String(w || '').trim();
        if (!text || seenWarnings.has(text)) continue;
        seenWarnings.add(text);
        warningsList.appendChild(el('li', { textContent: text }));
      }
      warningsPanel.hidden = seenWarnings.size === 0;
    }

    const bodyEl = el('div', { className: 'reader-body' });
    let lastSection = '';

    function appendContent(article) {
      const passages = Array.isArray(article.passages) ? article.passages : [];
      if (passages.length > 0) {
        for (const p of passages) {
          const sec = (p.section || '').trim();
          if (sec && sec !== lastSection) {
            lastSection = sec;
            bodyEl.appendChild(el('h2', { className: 'reader-section-heading', textContent: sec }));
          }
          if (p.text) {
            bodyEl.appendChild(el('p', { className: 'reader-passage', textContent: p.text }));
          }
        }
        return passages.length;
      }
      if (article.text) {
        const paras = String(article.text)
          .split(/\n{2,}/)
          .map((t) => t.trim())
          .filter(Boolean);
        for (const para of paras) {
          bodyEl.appendChild(el('p', { className: 'reader-passage', textContent: para }));
        }
        return paras.length;
      }
      return 0;
    }

    const statusLiveEl = el('div', { className: 'muted', 'aria-live': 'polite' });
    const loadMoreErrorEl = el('div');
    const footerElBox = el('div', { className: 'reader-footer' });

    let continuation = data.continuation || '';

    function renderFooter() {
      clear(footerElBox);
      if (continuation) {
        const loadMoreBtn = el('button', {
          type: 'button',
          className: 'btn btn-primary',
          textContent: 'Load more',
          onClick: async () => {
            clear(loadMoreErrorEl);
            loadMoreBtn.disabled = true;
            loadMoreBtn.textContent = 'Loading…';
            statusLiveEl.textContent = 'Loading more passages…';
            try {
              const { data: nextData } = await apiFetch('/api/v1/article-reads', {
                method: 'POST',
                json: { id: trimmedId, continuation },
                signal,
              });
              if (signal.aborted) return;

              appendWarnings(nextData.warnings);
              const added = appendContent(nextData);
              continuation = nextData.continuation || '';
              renderFooter();

              if (!continuation) {
                statusLiveEl.textContent =
                  added > 0 ? `Loaded ${added} more passage${added === 1 ? '' : 's'}. End of text.` : 'End of text.';
                announce('End of text.');
              } else {
                statusLiveEl.textContent = `Loaded ${added} more passage${added === 1 ? '' : 's'}.`;
                announce(statusLiveEl.textContent);
              }
            } catch (err) {
              if (err && err.name === 'AbortError') return;
              if (authMode === 'signin') return;

              loadMoreBtn.disabled = false;
              loadMoreBtn.textContent = 'Load more';
              statusLiveEl.textContent = '';
              clear(loadMoreErrorEl);
              loadMoreErrorEl.appendChild(
                renderErrorPanel(err, { heading: 'Could not load more passages' })
              );
            }
          },
        });
        footerElBox.appendChild(loadMoreBtn);
        footerElBox.appendChild(statusLiveEl);
      } else {
        footerElBox.appendChild(el('span', { className: 'end-marker', textContent: 'End of text' }));
        footerElBox.appendChild(statusLiveEl);
      }
    }

    appendWarnings(data.warnings);
    const initialCount = appendContent(data);
    if (initialCount === 0) {
      bodyEl.appendChild(el('p', { className: 'muted', textContent: 'No text passages were returned for this article.' }));
    }
    renderFooter();
    if (!continuation) {
      statusLiveEl.textContent = 'End of text.';
    }

    const metaItems = [];
    if (data.openalex_id) addMetaItem(metaItems, 'OpenAlex ID', el('code', { textContent: data.openalex_id }));
    const extUrl = safeExternalUrl(data.url);
    if (extUrl) {
      addMetaItem(
        metaItems,
        'Source URL',
        el('a', { href: extUrl, target: '_blank', rel: 'noopener noreferrer', textContent: extUrl })
      );
    }
    if (data.sha256) addMetaItem(metaItems, 'SHA-256', el('code', { textContent: data.sha256 }));

    const sourceBadges = [
      el('span', {
        className: sourceInfo.isAbstract ? 'badge badge-warning' : 'badge badge-success',
        textContent: sourceInfo.label,
      }),
    ];
    if (sourceInfo.raw) {
      sourceBadges.push(el('span', { className: 'badge', textContent: `Source: ${sourceInfo.raw}` }));
    }

    appEl.appendChild(
      el(
        'div',
        { className: 'view-stack' },
        el('a', { href: backHref, className: 'back-link', textContent: backLabel }),
        el(
          'article',
          { className: 'panel view-stack' },
          el(
            'div',
            { className: 'reader-meta' },
            el('div', { className: 'badge-row' }, sourceBadges),
            heading,
            metaItems.length > 0 ? el('dl', { className: 'meta-grid' }, metaItems) : null
          ),
          warningsPanel,
          bodyEl,
          loadMoreErrorEl,
          footerElBox
        )
      )
    );

    focusHeading(heading, title);
    if (!continuation) {
      announce('End of text.');
    }
  } catch (err) {
    if (err && err.name === 'AbortError') return;
    if (authMode === 'signin') return;

    clear(appEl);
    const errHeading = el('h1', { className: 'page-title', textContent: 'Article unavailable' });
    appEl.appendChild(
      el(
        'div',
        { className: 'view-stack' },
        el('a', { href: backHref, className: 'back-link', textContent: backLabel }),
        errHeading,
        renderErrorPanel(err, {
          heading: 'Could not read article',
          onRetry: () => renderReadView(trimmedId, signal),
        })
      )
    );
    focusHeading(errHeading, 'Article unavailable');
  }
}

// ---------------------------------------------------------------------------
// Account view (#/account)
// ---------------------------------------------------------------------------

async function renderAccountView(signal) {
  clear(appEl);

  const heading = el('h1', { className: 'page-title', textContent: 'Account' });
  appEl.appendChild(
    el(
      'div',
      { className: 'view-stack' },
      heading,
      renderLoadingPanel('Loading account status…')
    )
  );
  focusHeading(heading, 'Account');

  try {
    const { data } = await apiFetch('/api/v1/account', { signal });
    if (signal.aborted) return;

    clear(appEl);

    const loadedHeading = el('h1', { className: 'page-title', textContent: 'Account' });

    const memberText = data.member ? 'Yes' : 'No';
    const searchAllowedText =
      data.search_allowed === true ? 'Yes' : data.search_allowed === false ? 'No' : 'Unknown';

    const memberships = Array.isArray(data.memberships) ? data.memberships : [];
    const left = typeof data.downloads_left === 'number' ? data.downloads_left : 0;
    const limit = typeof data.downloads_limit === 'number' && data.downloads_limit > 0 ? data.downloads_limit : null;
    const windowHours =
      typeof data.window_hours === 'number' && data.window_hours > 0 ? data.window_hours : null;

    let quotaSummary;
    if (limit !== null && windowHours !== null) {
      quotaSummary = `${left} of ${limit} downloads left within ${windowHours} hours`;
    } else if (limit !== null) {
      quotaSummary = `${left} of ${limit} downloads left`;
    } else {
      quotaSummary = `${left} downloads left`;
    }

    const quotaCard = el(
      'section',
      { className: 'panel' },
      el('h2', { className: 'section-title', textContent: 'Fast downloads' }),
      el('p', { textContent: quotaSummary }),
      limit !== null
        ? el('progress', {
            className: 'quota-progress',
            value: Math.min(left, limit),
            max: limit,
            'aria-label': quotaSummary,
          })
        : null
    );

    const statusCard = el(
      'section',
      { className: 'panel' },
      el('h2', { className: 'section-title', textContent: 'Membership and access' }),
      el(
        'dl',
        { className: 'meta-grid' },
        el(
          'div',
          { className: 'meta-item' },
          el('dt', { textContent: 'Member' }),
          el(
            'dd',
            null,
            el('span', {
              className: data.member ? 'badge badge-success' : 'badge',
              textContent: memberText,
            })
          )
        ),
        el(
          'div',
          { className: 'meta-item' },
          el('dt', { textContent: 'Search allowed' }),
          el(
            'dd',
            null,
            el('span', {
              className:
                data.search_allowed === true
                  ? 'badge badge-success'
                  : data.search_allowed === false
                    ? 'badge badge-warning'
                    : 'badge',
              textContent: searchAllowedText,
            })
          )
        ),
        el(
          'div',
          { className: 'meta-item' },
          el('dt', { textContent: 'Memberships' }),
          el(
            'dd',
            null,
            memberships.length > 0
              ? el(
                  'div',
                  { className: 'badge-row' },
                  memberships.map((m) => el('span', { className: 'badge badge-accent', textContent: m }))
                )
              : 'None'
          )
        )
      )
    );

    const refreshBtn = el('button', {
      type: 'button',
      className: 'btn btn-secondary btn-sm',
      textContent: 'Refresh',
      onClick: () => renderAccountView(signal),
    });

    appEl.appendChild(
      el(
        'div',
        { className: 'view-stack' },
        el(
          'div',
          { className: 'actions-row' },
          loadedHeading,
          refreshBtn
        ),
        el('div', { className: 'account-grid' }, statusCard, quotaCard)
      )
    );

    focusHeading(loadedHeading, 'Account');
    announce(`Account status loaded. ${quotaSummary}.`);
  } catch (err) {
    if (err && err.name === 'AbortError') return;
    if (authMode === 'signin') return;

    clear(appEl);
    const errHeading = el('h1', { className: 'page-title', textContent: 'Account' });
    appEl.appendChild(
      el(
        'div',
        { className: 'view-stack' },
        errHeading,
        renderErrorPanel(err, {
          heading: 'Could not load account status',
          onRetry: () => renderAccountView(signal),
        })
      )
    );
    focusHeading(errHeading, 'Account');
  }
}

// ---------------------------------------------------------------------------
// Startup & event wiring
// ---------------------------------------------------------------------------

async function boot() {
  if (activeController) {
    activeController.abort();
    activeController = null;
  }

  authMode = 'loading';
  updateShellVisibility();
  clear(appEl);
  appEl.appendChild(
    el(
      'div',
      { className: 'auth-shell' },
      renderLoadingPanel('Connecting to aakb…')
    )
  );

  try {
    // First call is always GET /api/v1/capabilities with no Authorization header.
    const { data } = await apiFetch('/api/v1/capabilities', {
      tokenOverride: null,
      allowUnauthorized: true,
    });
    capabilities = data || { version: '', openalex: false, auth_required: false };
    authToken = null;
    authMode = 'tokenless';
    updateShellVisibility();
    routeCurrentHash();
  } catch (err) {
    if (err instanceof ApiError && err.status === 401) {
      authToken = null;
      authMode = 'signin';
      signInMessage = null;
      updateShellVisibility();
      renderSignInView();
      return;
    }
    if (err instanceof ApiError && err.status === 403 && err.code === 'TOKEN_REQUIRED') {
      authToken = null;
      authMode = 'token_required';
      updateShellVisibility();
      renderTokenRequiredView(err);
      return;
    }
    authToken = null;
    authMode = 'error';
    updateShellVisibility();
    renderStartupErrorView(err);
  }
}

signOutBtn.addEventListener('click', () => {
  if (activeController) {
    activeController.abort();
    activeController = null;
  }
  authToken = null;
  recordCache.clear();
  authMode = 'signin';
  signInMessage = 'You have signed out.';
  updateShellVisibility();
  announce('Signed out.');
  renderSignInView();
});

window.addEventListener('hashchange', () => {
  routeCurrentHash();
});

boot();
