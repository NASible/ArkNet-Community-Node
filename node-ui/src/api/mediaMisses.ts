export type MediaMissPolicy = 'MANUAL' | 'ASK' | 'AUTOMATIC';
export interface MediaMissNotification { media_id: string; title: string; size: number; miss_count: number }
export interface MediaMisses { policy: MediaMissPolicy; notifications: MediaMissNotification[] }

export class MediaMissError extends Error {}

const policies: MediaMissPolicy[] = ['MANUAL', 'ASK', 'AUTOMATIC'];
const invalid = () => new MediaMissError('The Node Agent returned an invalid media-miss response.');
const record = (value: unknown): value is Record<string, unknown> => typeof value === 'object' && value !== null && !Array.isArray(value);
const count = (value: unknown): value is number => typeof value === 'number' && Number.isSafeInteger(value) && value >= 0;
const isPolicy = (value: unknown): value is MediaMissPolicy => policies.includes(value as MediaMissPolicy);

async function json(response: Response): Promise<unknown> {
  try { return await response.json(); } catch { throw invalid(); }
}

function notification(value: unknown): MediaMissNotification {
  if (!record(value) || Object.keys(value).length !== 4 || typeof value.media_id !== 'string' || !/^media_[0-9a-f]{64}$/.test(value.media_id) ||
    typeof value.title !== 'string' || !count(value.size) || !count(value.miss_count) || value.miss_count < 1) throw invalid();
  return { media_id: value.media_id, title: value.title, size: value.size, miss_count: value.miss_count };
}

export async function fetchMediaMisses(signal?: AbortSignal): Promise<MediaMisses> {
  const response = await fetch('/api/v1/media-misses', { signal, cache: 'no-store' });
  if (!response.ok) throw new MediaMissError('Missed media could not be loaded. Check the Node Agent and retry.');
  const body = await json(response);
  if (!record(body) || !isPolicy(body.policy) || !Array.isArray(body.notifications)) throw invalid();
  return { policy: body.policy, notifications: body.notifications.map(notification) };
}

export async function ignoreMediaMiss(id: string, signal?: AbortSignal): Promise<void> {
  if (!/^media_[0-9a-f]{64}$/.test(id)) throw invalid();
  const response = await fetch(`/api/v1/media-misses/${encodeURIComponent(id)}/ignore`, {
    method: 'POST', headers: { 'Content-Type': 'application/json' }, body: '{}', signal, cache: 'no-store',
  });
  if (response.status !== 204) throw new MediaMissError('The notification could not be ignored. Refresh and retry.');
}

async function policyRequest(options?: RequestInit): Promise<MediaMissPolicy> {
  const response = await fetch('/api/v1/settings/media-miss-policy', { cache: 'no-store', ...options });
  if (!response.ok) throw new MediaMissError('The missed-media policy could not be saved. Try again.');
  const body = await json(response);
  if (!record(body) || Object.keys(body).length !== 1 || !isPolicy(body.policy)) throw invalid();
  return body.policy;
}

export const fetchMediaMissPolicy = (signal?: AbortSignal) => policyRequest({ signal });
export const updateMediaMissPolicy = (policy: MediaMissPolicy, signal?: AbortSignal) => policyRequest({
  method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ policy }), signal,
});
