// Nexus Hub API Client
// All writes use Content-Type: application/json for the hub's CSRF Origin validation.
// All requests include session credentials.

export async function request(path, options = {}) {
  const { method = 'GET', json, headers = {}, ...rest } = options;
  const opts = {
    method,
    headers: { ...headers },
    credentials: 'include',
    ...rest,
  };

  if (json !== undefined) {
    opts.body = JSON.stringify(json);
    opts.headers['Content-Type'] = 'application/json';
  }

  const res = await fetch(path, opts);

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
export const postLogin = (creds) => request('/hub/api/login', { method: 'POST', json: creds });
export const postLogout = () => request('/hub/api/logout', { method: 'POST', json: {} });
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
