// Nexus Hub API Client
// The dashboard signs in like the app: a 15-minute access token and a 30-day
// rotating refresh token, both kept in localStorage so a reload stays signed in.
// Writes use Content-Type: application/json.

const TOKENS_KEY = 'nexus.hub.tokens';

function loadTokens() {
  try {
    const t = JSON.parse(localStorage.getItem(TOKENS_KEY) || 'null');
    return t && t.access && t.refresh ? t : null;
  } catch {
    return null;
  }
}

export function saveTokens(res) {
  try {
    localStorage.setItem(TOKENS_KEY, JSON.stringify({ access: res.access_token, refresh: res.refresh_token }));
  } catch {
    // Storage blocked: this tab stays signed in until reload.
  }
  memTokens = { access: res.access_token, refresh: res.refresh_token };
}

export function clearTokens() {
  try {
    localStorage.removeItem(TOKENS_KEY);
  } catch {
    // ignore
  }
  memTokens = null;
}

let memTokens = loadTokens();
const tokens = () => loadTokens() || memTokens;

// One refresh at a time: refresh tokens rotate, and reusing one revokes the family.
let refreshing = null;
async function refreshTokens(rejectedAccess) {
  const cur = tokens();
  if (!cur) return false;
  if (cur.access !== rejectedAccess) return true; // another tab or call already refreshed
  if (!refreshing) {
    refreshing = (async () => {
      const res = await fetch('/hub/api/auth/refresh', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ refresh_token: cur.refresh }),
      });
      if (!res.ok) {
        if (res.status === 401) clearTokens();
        return false;
      }
      saveTokens(await res.json());
      return true;
    })().catch(() => false).finally(() => { refreshing = null; });
  }
  return refreshing;
}

async function send(path, opts) {
  const t = tokens();
  const headers = { ...opts.headers };
  if (t) headers.Authorization = `Bearer ${t.access}`;
  const res = await fetch(path, { ...opts, headers });
  return { res, access: t?.access };
}

export async function request(path, options = {}) {
  const { method = 'GET', json, headers = {}, ...rest } = options;
  const opts = {
    method,
    headers: { ...headers },
    ...rest,
  };

  if (json !== undefined) {
    opts.body = JSON.stringify(json);
    opts.headers['Content-Type'] = 'application/json';
  }

  let { res, access } = await send(path, opts);
  if (res.status === 401 && access && await refreshTokens(access)) {
    ({ res } = await send(path, opts));
  }


  // Parse JSON response body if present
  let data = null;
  const text = await res.text();
  if (text) {
    try {
      data = JSON.parse(text);
    } catch {
      data = { raw: text };
    }
  }

  // Older endpoints answer {"error":"message"}, newer ones {"error":{"code","message"}}.
  const errText = typeof data?.error === 'object' && data.error !== null
    ? (data.error.message || data.error.code)
    : data?.error;

  if (res.status === 401) {
    window.dispatchEvent(new CustomEvent('hub:unauthorized'));
    const err = new Error(errText || 'unauthorized');
    err.status = 401;
    throw err;
  }

  if (res.status === 403 && data?.error === 'password change required') {
    window.dispatchEvent(new CustomEvent('hub:must_change'));
    const err = new Error('password change required');
    err.status = 403;
    err.mustChange = true;
    throw err;
  }

  if (!res.ok) {
    const err = new Error(errText || `HTTP ${res.status}`);
    err.status = res.status;
    err.data = data;
    throw err;
  }

  return data;
}

// Hub Auth & User API
export const getSetupStatus = () => request('/hub/api/setup');
export const postSetup = (form) => request('/hub/api/setup', { method: 'POST', json: form });
export const postToken = (creds) => request('/hub/api/auth/token', { method: 'POST', json: creds });
export const postLogout = () => {
  const t = tokens();
  return request('/hub/api/auth/logout', { method: 'POST', json: { refresh_token: t?.refresh || '' } });
};
export const hasTokens = () => !!tokens();
export const getMe = () => request('/hub/api/me');
export const changePassword = (current, next) => request('/hub/api/me/password', { method: 'POST', json: { current, new: next } });

// Hub Devices API
export const getDevices = () => request('/hub/api/devices');
export const patchDevice = (id, name) => request(`/hub/api/devices/${encodeURIComponent(id)}`, { method: 'PATCH', json: { name } });
export const deleteDevice = (id) => request(`/hub/api/devices/${encodeURIComponent(id)}`, { method: 'DELETE', json: {} });
export const createPairingCode = () => request('/hub/api/pairing', { method: 'POST', json: {} });
export const getPairingCodes = () => request('/hub/api/pairing');
export const cancelPairingCode = (code) => request(`/hub/api/pairing/${encodeURIComponent(code)}`, { method: 'DELETE', json: {} });

// Hub Users Admin API
export const getUsers = () => request('/hub/api/users');
export const createUser = (user) => request('/hub/api/users', { method: 'POST', json: user });
export const resetUserPassword = (id) => request(`/hub/api/users/${encodeURIComponent(id)}/reset`, { method: 'POST', json: {} });
export const deleteUser = (id) => request(`/hub/api/users/${encodeURIComponent(id)}`, { method: 'DELETE', json: {} });
const userAction = (id, action) => request(`/hub/api/users/${encodeURIComponent(id)}/${action}`, { method: 'POST', json: {} });
export const approveUser = (id) => userAction(id, 'approve');
export const rejectUser = (id) => userAction(id, 'reject');
export const disableUser = (id) => userAction(id, 'disable');
export const enableUser = (id) => userAction(id, 'enable');
