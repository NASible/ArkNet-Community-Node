// Global member sign-in through Core (Prompt 46B, docs/global-login-assertions.md).
// The global password is typed only on Core's own page; this UI never shows a
// password field for global members. The Node's login state and device
// credential live in HttpOnly cookies; the member session token is kept only
// in memory, never in browser storage.

export interface MemberSession {
  member_id: string;
  kind: 'GLOBAL';
  permissions: string[];
  offline: boolean;
  token: string;
  expires_at: string;
}

export type LoginStart = { kind: 'redirect'; url: string } | { kind: 'session'; session: MemberSession };

export class MemberError extends Error {
  constructor(public readonly code: string, message: string) { super(message); this.name = 'MemberError'; }
}

const messages: Record<string, string> = {
  invalid_global_login: 'This sign-in did not complete or was already used. Start again.',
  offline_login_rejected: 'ArkNet Core is unreachable and this device cannot sign in offline (its offline period ended or it never signed in here). Sign in online when Core is reachable.',
  global_authentication_unavailable: 'ArkNet Core is unreachable and this browser has not signed in on this Node before. Try again when Core is reachable.',
  global_login_not_configured: 'Member sign-in is not configured on this Node. Ask the Node operator.',
  invalid_callback_url: 'This Node does not accept sign-ins at this address. Use the address the operator configured.',
};

const record = (value: unknown): value is Record<string, unknown> => typeof value === 'object' && value !== null && !Array.isArray(value);

function memberSession(value: unknown): MemberSession {
  if (!record(value) || typeof value.member_id !== 'string' || !/^member_[0-9a-f]{64}$/.test(value.member_id) ||
    typeof value.token !== 'string' || typeof value.expires_at !== 'string' || typeof value.offline !== 'boolean') {
    throw new MemberError('invalid-payload', 'The Agent returned an invalid session. Check UI and Agent compatibility.');
  }
  return value as unknown as MemberSession;
}

async function call(path: string, method: string, body?: unknown, token?: string): Promise<{ status: number; data: unknown }> {
  const headers: Record<string, string> = {};
  if (body !== undefined) headers['Content-Type'] = 'application/json';
  if (token) headers.Authorization = 'Bearer ' + token;
  const response = await fetch(path, { method, cache: 'no-store', credentials: 'same-origin', headers,
    body: body === undefined ? undefined : JSON.stringify(body) });
  let data: unknown = null;
  if (response.status !== 204) {
    try { data = await response.json(); } catch { data = null; }
  }
  return { status: response.status, data };
}

function failure(status: number, data: unknown): MemberError {
  const code = record(data) && typeof data.error === 'string' ? data.error : '';
  return new MemberError(code || 'http-error', messages[code] ?? `Sign-in failed (HTTP ${status}). Try again.`);
}

export function callbackURL(origin = window.location.origin): string {
  return `${origin}/login/callback`;
}

export async function startLogin(): Promise<LoginStart> {
  const { status, data } = await call('/api/v1/global-users/login', 'POST', { callback_url: callbackURL() });
  if (status === 201) return { kind: 'session', session: memberSession(data) };
  if (status !== 200) throw failure(status, data);
  const url = record(data) && typeof data.authorize_url === 'string' ? data.authorize_url : '';
  let parsed: URL | null = null;
  try { parsed = new URL(url); } catch { parsed = null; }
  if (!parsed || (parsed.protocol !== 'https:' && parsed.protocol !== 'http:')) {
    throw new MemberError('invalid-payload', 'The Agent returned an invalid sign-in address. Check UI and Agent compatibility.');
  }
  return { kind: 'redirect', url };
}

export async function completeLogin(assertion: string): Promise<MemberSession> {
  const { status, data } = await call('/api/v1/global-users/login/complete', 'POST', { assertion });
  if (status !== 201) throw failure(status, data);
  return memberSession(data);
}

export async function signOut(token: string): Promise<void> {
  const { status, data } = await call('/api/v1/global-users/session', 'DELETE', undefined, token);
  if (status !== 204) throw failure(status, data);
}

// readCallbackAssertion takes the assertion from the URL fragment and removes
// it from the address bar and history before anything else runs.
export function readCallbackAssertion(): string | null {
  if (window.location.pathname !== '/login/callback') return null;
  const fragment = new URLSearchParams(window.location.hash.replace(/^#/, ''));
  window.history.replaceState(null, '', '/member');
  return fragment.get('assertion');
}
