// Operator sign-in for the Node management UI (Prompt 45B). The session lives
// only in the Agent's HttpOnly cookie; nothing here is written to browser
// storage, and no token ever reaches JavaScript.

export interface OperatorSession {
  operator_id: string;
  username: string;
  authenticated_at: string;
  idle_expires_at: string;
  expires_at: string;
}

export interface SetupStart {
  enrollment_token: string;
  purpose: 'SETUP' | 'RECOVERY';
  username: string;
  totp_uri: string;
  totp_secret: string;
  expires_at: string;
}

export type SessionState =
  | { kind: 'signed-in'; session: OperatorSession }
  | { kind: 'signed-out' }
  | { kind: 'setup-required' };

export class OperatorError extends Error {
  constructor(public readonly code: string, message: string) { super(message); this.name = 'OperatorError'; }
}

const messages: Record<string, string> = {
  invalid_credentials: 'Sign-in failed. Check the username, password and authenticator code.',
  invalid_setup_code: 'That setup or recovery code is not valid. Codes work once and expire after 1 hour; run `sudo arknet-node operator setup-code` for a new one.',
  invalid_username_or_password: 'Usernames are 3–32 characters (letters, digits, “.”, “_” or “-”); passwords are 12–256 characters.',
  username_taken: 'An operator with that username already exists. Choose another, or use `sudo arknet-node operator recover NAME`.',
  invalid_enrollment: 'Setup took too long or was already completed. Start again with a new setup code.',
  invalid_totp: 'That authenticator code is not correct. Check the device time and try the current code.',
  too_many_attempts: 'Too many failed attempts. Wait a minute and try again.',
  operators_unavailable: 'The Node Agent cannot reach its database. Check the Node and retry.',
};

const record = (value: unknown): value is Record<string, unknown> => typeof value === 'object' && value !== null && !Array.isArray(value);

function session(value: unknown): OperatorSession {
  if (!record(value) || typeof value.operator_id !== 'string' || !/^op_[0-9a-f]{64}$/.test(value.operator_id) ||
    typeof value.username !== 'string' || typeof value.expires_at !== 'string' || typeof value.authenticated_at !== 'string' ||
    typeof value.idle_expires_at !== 'string') throw new OperatorError('invalid-payload', 'The Agent returned an invalid session. Check UI and Agent compatibility.');
  return value as unknown as OperatorSession;
}

async function call(path: string, method: string, body?: unknown): Promise<{ status: number; data: unknown }> {
  const response = await fetch(path, {
    method, cache: 'no-store', credentials: 'same-origin',
    headers: body === undefined ? undefined : { 'Content-Type': 'application/json' },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  let data: unknown = null;
  if (response.status !== 204) {
    try { data = await response.json(); } catch { data = null; }
  }
  return { status: response.status, data };
}

function failure(status: number, data: unknown): OperatorError {
  const code = record(data) && typeof data.error === 'string' ? data.error : '';
  return new OperatorError(code || 'http-error', messages[code] ?? `Request failed (HTTP ${status}). Check the Node Agent and retry.`);
}

export async function currentSession(): Promise<SessionState> {
  const { status, data } = await call('/api/v1/operator/session', 'GET');
  if (status === 200) return { kind: 'signed-in', session: session(data) };
  if (status === 401) return record(data) && data.error === 'operator_setup_required' ? { kind: 'setup-required' } : { kind: 'signed-out' };
  throw failure(status, data);
}

export async function signIn(username: string, password: string, second: { totp: string } | { recovery_code: string }): Promise<OperatorSession> {
  const { status, data } = await call('/api/v1/operator/sessions', 'POST', { username, password, ...second });
  if (status !== 201) throw failure(status, data);
  return session(data);
}

export async function signOut(): Promise<void> {
  const { status, data } = await call('/api/v1/operator/session', 'DELETE');
  if (status !== 204 && status !== 401) throw failure(status, data);
}

export async function beginSetup(setupCode: string, username: string, password: string): Promise<SetupStart> {
  const { status, data } = await call('/api/v1/operator/setup', 'POST', { setup_code: setupCode, username, password });
  if (status !== 201 || !record(data) || typeof data.enrollment_token !== 'string' || typeof data.totp_secret !== 'string' ||
    typeof data.totp_uri !== 'string') throw failure(status, data);
  return data as unknown as SetupStart;
}

export async function confirmSetup(enrollmentToken: string, totp: string): Promise<{ session: OperatorSession; recoveryCodes: string[] }> {
  const { status, data } = await call('/api/v1/operator/setup/confirm', 'POST', { enrollment_token: enrollmentToken, totp });
  if (status !== 201 || !record(data) || !Array.isArray(data.recovery_codes)) throw failure(status, data);
  return { session: session(data.session), recoveryCodes: data.recovery_codes.map(String) };
}
