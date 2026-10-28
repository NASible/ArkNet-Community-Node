export type ReleaseChannel = 'stable' | 'development';
export type DiscoveryState = 'NOT_CONFIGURED' | 'NO_CURRENT_RELEASE' | 'NOT_CHECKED' | 'UP_TO_DATE' | 'UPDATE_AVAILABLE' | 'CHECK_FAILED';
export interface AvailableRelease { version: string; channel: ReleaseChannel; published_at: string }
export interface UpdateStatus {
  current_version: string | null;
  channel: ReleaseChannel | null;
  available: AvailableRelease | null;
  automatic_updates: boolean;
  automatic_install_supported: boolean;
  discovery: { state: DiscoveryState; checked_at: string | null };
}

export class UpdatesError extends Error {}

const states: DiscoveryState[] = ['NOT_CONFIGURED', 'NO_CURRENT_RELEASE', 'NOT_CHECKED', 'UP_TO_DATE', 'UPDATE_AVAILABLE', 'CHECK_FAILED'];
const version = /^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$/;
const invalid = () => new UpdatesError('The Node Agent returned an invalid update response.');
const record = (v: unknown): v is Record<string, unknown> => typeof v === 'object' && v !== null && !Array.isArray(v);
const channel = (v: unknown): v is ReleaseChannel => v === 'stable' || v === 'development';
const timestamp = (v: unknown): v is string => typeof v === 'string' && !Number.isNaN(Date.parse(v));
const exact = (v: Record<string, unknown>, keys: string[]) => Object.keys(v).length === keys.length && keys.every(k => k in v);

function parseStatus(body: unknown): UpdateStatus {
  if (!record(body) || !exact(body, ['current_version', 'channel', 'available', 'automatic_updates', 'automatic_install_supported', 'discovery'])) throw invalid();
  const { current_version, channel: ch, available, automatic_updates, automatic_install_supported, discovery } = body;
  if (!(current_version === null || (typeof current_version === 'string' && version.test(current_version))) ||
    !(ch === null || channel(ch)) || typeof automatic_updates !== 'boolean' || typeof automatic_install_supported !== 'boolean' ||
    !record(discovery) || !exact(discovery, ['state', 'checked_at']) || !states.includes(discovery.state as DiscoveryState) ||
    !(discovery.checked_at === null || timestamp(discovery.checked_at))) throw invalid();
  let parsed: AvailableRelease | null = null;
  if (available !== null) {
    if (!record(available) || !exact(available, ['version', 'channel', 'published_at']) || typeof available.version !== 'string' ||
      !version.test(available.version) || !channel(available.channel) || !timestamp(available.published_at)) throw invalid();
    parsed = { version: available.version, channel: available.channel, published_at: available.published_at };
  }
  return {
    current_version, channel: ch, available: parsed, automatic_updates, automatic_install_supported,
    discovery: { state: discovery.state as DiscoveryState, checked_at: discovery.checked_at },
  };
}

async function json(response: Response): Promise<unknown> {
  try { return await response.json(); } catch { throw invalid(); }
}

export async function fetchUpdates(signal?: AbortSignal): Promise<UpdateStatus> {
  const response = await fetch('/api/v1/updates', { signal, cache: 'no-store' });
  if (!response.ok) throw new UpdatesError('Update status could not be loaded. Check the Node Agent and retry.');
  return parseStatus(await json(response));
}

export async function checkForUpdates(signal?: AbortSignal): Promise<UpdateStatus> {
  const response = await fetch('/api/v1/updates/check', {
    method: 'POST', headers: { 'Content-Type': 'application/json' }, body: '{}', signal, cache: 'no-store',
  });
  if (!response.ok) throw new UpdatesError('The update check could not run. Check the Node Agent and retry.');
  return parseStatus(await json(response));
}

export async function setAutomaticUpdates(enabled: boolean, signal?: AbortSignal): Promise<boolean> {
  const response = await fetch('/api/v1/settings/automatic-updates', {
    method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ enabled }), signal, cache: 'no-store',
  });
  if (!response.ok) throw new UpdatesError('The automatic update setting could not be saved. Try again.');
  const body = await json(response);
  if (!record(body) || !exact(body, ['enabled']) || body.enabled !== enabled) throw invalid();
  return body.enabled;
}
