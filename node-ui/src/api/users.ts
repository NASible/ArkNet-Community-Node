export type AccountStatus = 'ACTIVE' | 'DISABLED';
export interface LocalUser { user_id: string; username: string; kind: 'LOCAL'; status: AccountStatus; created_at: string }

export class UsersError extends Error {
  constructor(public readonly kind: string, message: string) { super(message); this.name = 'UsersError'; }
}

const invalid = () => new UsersError('invalid-payload', 'The Agent returned an invalid users response. Check UI and Agent compatibility.');
const record = (value: unknown): value is Record<string, unknown> => typeof value === 'object' && value !== null && !Array.isArray(value);
const timestamp = (value: unknown): value is string => typeof value === 'string' && !Number.isNaN(Date.parse(value));

function localUser(value: unknown): LocalUser {
  if (!record(value) || typeof value.user_id !== 'string' || !/^local_[0-9a-f]{64}$/.test(value.user_id) ||
    typeof value.username !== 'string' || value.kind !== 'LOCAL' || !['ACTIVE', 'DISABLED'].includes(value.status as string) ||
    !timestamp(value.created_at)) throw invalid();
  return { user_id: value.user_id, username: value.username, kind: 'LOCAL', status: value.status as AccountStatus, created_at: value.created_at };
}

const messages: Record<string, string> = {
  username_taken: 'A local user with that username already exists on this node.',
  invalid_username: 'Usernames are 3–32 characters: letters, digits, “.”, “_” or “-”, starting with a letter or digit.',
  invalid_password: 'Passwords must be 12–256 characters.',
  local_user_not_found: 'That local user no longer exists.',
  local_users_unavailable: 'The local user database is unavailable. Check the Node Agent and retry.',
};

async function call(path: string, init?: RequestInit): Promise<unknown> {
  const response = await fetch(path, { cache: 'no-store', ...init });
  let data: unknown;
  try { data = await response.json(); } catch { throw invalid(); }
  if (!response.ok) {
    const code = record(data) && typeof data.error === 'string' ? data.error : '';
    throw new UsersError(code || 'http-error', messages[code] ?? `Users request failed (HTTP ${response.status}). Check the Node Agent and retry.`);
  }
  return data;
}

export async function fetchLocalUsers(signal?: AbortSignal): Promise<LocalUser[]> {
  const data = await call('/api/v1/local-users', { signal });
  if (!record(data) || !Array.isArray(data.users)) throw invalid();
  return data.users.map(localUser);
}

export async function createLocalUser(username: string, password: string): Promise<LocalUser> {
  return localUser(await call('/api/v1/local-users', {
    method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ username, password }),
  }));
}

export async function setLocalUserEnabled(id: string, enabled: boolean): Promise<LocalUser> {
  if (!/^local_[0-9a-f]{64}$/.test(id)) throw invalid();
  return localUser(await call(`/api/v1/local-users/${id}/${enabled ? 'enable' : 'disable'}`, { method: 'POST' }));
}
