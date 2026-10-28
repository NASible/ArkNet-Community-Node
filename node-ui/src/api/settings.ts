export type PeerServingSetting = { enabled: boolean };

export class SettingsError extends Error {}

function parseSetting(value: unknown): PeerServingSetting {
  if (!value || typeof value !== 'object' || Object.keys(value).length !== 1 || typeof (value as { enabled?: unknown }).enabled !== 'boolean') {
    throw new SettingsError('The Node Agent returned an invalid response.');
  }
  return value as PeerServingSetting;
}

async function request(options?: RequestInit): Promise<PeerServingSetting> {
  const response = await fetch('/api/v1/settings/peer-serving', { signal: options?.signal, ...options });
  if (!response.ok) throw new SettingsError('The Peer Serving setting could not be saved. Try again.');
  let body: unknown;
  try { body = await response.json(); } catch { throw new SettingsError('The Node Agent returned an invalid response.'); }
  return parseSetting(body);
}

export const fetchPeerServing = (signal?: AbortSignal) => request({ signal });
export const updatePeerServing = (enabled: boolean, signal?: AbortSignal) => request({
  method: 'PUT',
  headers: { 'Content-Type': 'application/json' },
  body: JSON.stringify({ enabled }),
  signal,
});
