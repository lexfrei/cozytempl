// API client with caching, retries, and offline resilience

const API_BASE = '/api';
const MAX_RETRIES = 3;
const RETRY_DELAYS = [1000, 2000, 4000]; // exponential backoff
const CACHE_TTL = 30000; // 30 seconds default cache
const STALE_TTL = 300000; // 5 minutes — serve stale if no connection

class ApiCache {
  constructor() {
    this._cache = new Map();
  }

  get(key) {
    const entry = this._cache.get(key);
    if (!entry) return null;

    const age = Date.now() - entry.timestamp;
    if (age < CACHE_TTL) return { data: entry.data, fresh: true };
    if (age < STALE_TTL) return { data: entry.data, fresh: false };

    this._cache.delete(key);
    return null;
  }

  set(key, data) {
    this._cache.set(key, { data, timestamp: Date.now() });
  }

  invalidate(prefix) {
    for (const key of this._cache.keys()) {
      if (key.startsWith(prefix)) {
        this._cache.delete(key);
      }
    }
  }

  clear() {
    this._cache.clear();
  }
}

const cache = new ApiCache();

async function sleep(ms) {
  return new Promise(resolve => setTimeout(resolve, ms));
}

function isRetryable(status) {
  return status === 0 || status === 408 || status === 429 ||
    status === 502 || status === 503 || status === 504;
}

/**
 * Make an API request with caching and retries.
 *
 * Options:
 *   method: HTTP method (default GET)
 *   body: request body (will be JSON-stringified)
 *   cache: enable caching (default true for GET)
 *   cacheTTL: custom cache TTL in ms
 *   retries: number of retries (default MAX_RETRIES for GET, 1 for mutations)
 *   signal: AbortSignal for cancellation
 */
async function api(path, options = {}) {
  const method = (options.method || 'GET').toUpperCase();
  const isRead = method === 'GET';
  const useCache = options.cache !== undefined ? options.cache : isRead;
  const retries = options.retries !== undefined ? options.retries : (isRead ? MAX_RETRIES : 1);

  // Check cache first for reads
  if (useCache && isRead) {
    const cached = cache.get(path);
    if (cached && cached.fresh) {
      return cached.data;
    }
    // Keep stale data as fallback
    var staleData = cached ? cached.data : null;
  }

  const fetchOptions = {
    method,
    headers: { 'Content-Type': 'application/json' },
  };
  if (options.body) {
    fetchOptions.body = typeof options.body === 'string' ? options.body : JSON.stringify(options.body);
  }
  if (options.signal) {
    fetchOptions.signal = options.signal;
  }

  let lastError = null;

  for (let attempt = 0; attempt <= retries; attempt++) {
    try {
      const resp = await fetch(API_BASE + path, fetchOptions);

      if (resp.status === 401) {
        window.location.href = '/auth/login';
        return null;
      }

      if (!resp.ok) {
        if (isRetryable(resp.status) && attempt < retries) {
          await sleep(RETRY_DELAYS[attempt] || RETRY_DELAYS[RETRY_DELAYS.length - 1]);
          continue;
        }
        const body = await resp.json().catch(() => ({ error: resp.statusText }));
        throw new ApiError(body.error || resp.statusText, resp.status);
      }

      if (resp.status === 204) {
        // Invalidate related cache on successful mutations
        if (!isRead) cache.invalidate(path.split('/').slice(0, -1).join('/'));
        return null;
      }

      const data = await resp.json();

      // Cache successful GET responses
      if (isRead && useCache) {
        cache.set(path, data);
      }

      // Invalidate related cache on successful mutations
      if (!isRead) {
        cache.invalidate(path.replace(/\/[^/]+$/, ''));
      }

      return data;
    } catch (err) {
      if (err instanceof ApiError) throw err;
      if (err.name === 'AbortError') throw err;

      lastError = err;

      // Network error — retry
      if (attempt < retries) {
        await sleep(RETRY_DELAYS[attempt] || RETRY_DELAYS[RETRY_DELAYS.length - 1]);
        continue;
      }
    }
  }

  // All retries exhausted — return stale data if available
  if (isRead && staleData !== undefined && staleData !== null) {
    console.warn('Serving stale data for', path);
    return staleData;
  }

  throw lastError || new ApiError('Network error', 0);
}

class ApiError extends Error {
  constructor(message, status) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
  }
}

// Invalidate cache for a tenant's apps (after create/update/delete)
function invalidateApps(tenant) {
  cache.invalidate('/tenants/' + tenant + '/apps');
  cache.invalidate('/tenants');
}

export { api, cache, invalidateApps, ApiError, API_BASE };
