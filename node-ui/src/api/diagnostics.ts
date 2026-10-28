export type ProbeStatus = 'ok' | 'dns' | 'refused' | 'timeout' | 'unreachable' | 'tls' | 'server_error' | 'error' | 'not_configured';
export type HealthStatus = 'healthy' | 'unhealthy' | 'not_configured';

export interface Diagnostics {
  generated_at: string;
  version: { agent: string; release: { version: string; channel: 'stable' | 'development' } | null };
  agent: { status: 'healthy' };
  database: { status: 'healthy' | 'unhealthy' };
  jellyfin: { status: HealthStatus };
  core: {
    status: ProbeStatus; host?: string; http_status?: number;
    node_state: string; enrollment_state: string; connectivity: string; last_contact_at: string | null;
  };
  tracker: { status: ProbeStatus; host?: string };
  tailscale: { status: 'detected' | 'not_detected' };
  storage: { status: 'healthy' | 'unavailable'; kind?: 'media' | 'docker'; total_bytes: number; free_bytes: number };
  media: { status: 'healthy' | 'unavailable'; allocation_bytes: number; reserved_bytes: number; selections: number };
  transfers: { status: 'healthy' | 'unavailable' | 'not_configured'; active: number; paused: number; failed: number; completed: number };
}

export class DiagnosticsError extends Error {
  constructor(public readonly agentUnreachable: boolean, message: string) { super(message); }
}

const probes: ProbeStatus[] = ['ok', 'dns', 'refused', 'timeout', 'unreachable', 'tls', 'server_error', 'error', 'not_configured'];
const record = (v: unknown): v is Record<string, unknown> => typeof v === 'object' && v !== null && !Array.isArray(v);
const count = (v: unknown) => typeof v === 'number' && Number.isSafeInteger(v) && v >= 0;
const text = (v: unknown) => typeof v === 'string';
const oneOf = <T extends string>(v: unknown, values: readonly T[]): v is T => values.includes(v as T);
const optionalHost = (v: unknown) => v === undefined || (typeof v === 'string' && v.length <= 300);

function valid(d: unknown): d is Diagnostics {
  if (!record(d)) return false;
  const { version, agent, database, jellyfin, core, tracker, tailscale, storage, media, transfers } = d;
  return text(d.generated_at) &&
    record(version) && text(version.agent) &&
    (version.release === null || (record(version.release) && text(version.release.version) && oneOf(version.release.channel, ['stable', 'development']))) &&
    record(agent) && agent.status === 'healthy' &&
    record(database) && oneOf(database.status, ['healthy', 'unhealthy']) &&
    record(jellyfin) && oneOf(jellyfin.status, ['healthy', 'unhealthy', 'not_configured']) &&
    record(core) && oneOf(core.status, probes) && optionalHost(core.host) && (core.http_status === undefined || count(core.http_status)) &&
    text(core.node_state) && text(core.enrollment_state) && text(core.connectivity) && (core.last_contact_at === null || text(core.last_contact_at)) &&
    record(tracker) && oneOf(tracker.status, probes) && optionalHost(tracker.host) &&
    record(tailscale) && oneOf(tailscale.status, ['detected', 'not_detected']) &&
    record(storage) && oneOf(storage.status, ['healthy', 'unavailable']) && count(storage.total_bytes) && count(storage.free_bytes) &&
    record(media) && oneOf(media.status, ['healthy', 'unavailable']) && typeof media.allocation_bytes === 'number' &&
    Number.isSafeInteger(media.allocation_bytes) && media.allocation_bytes >= -1 && count(media.reserved_bytes) && count(media.selections) &&
    record(transfers) && oneOf(transfers.status, ['healthy', 'unavailable', 'not_configured']) &&
    count(transfers.active) && count(transfers.paused) && count(transfers.failed) && count(transfers.completed);
}

export async function fetchDiagnostics(signal?: AbortSignal): Promise<Diagnostics> {
  let response: Response;
  try {
    response = await fetch('/api/v1/diagnostics', { signal, cache: 'no-store' });
  } catch (cause) {
    if (signal?.aborted) throw cause;
    throw new DiagnosticsError(true, 'agent-unreachable');
  }
  if (response.status === 502 || response.status === 504) throw new DiagnosticsError(true, 'agent-unreachable');
  if (!response.ok) throw new DiagnosticsError(false, `http-${response.status}`);
  let body: unknown;
  try { body = await response.json(); } catch { throw new DiagnosticsError(false, 'invalid'); }
  if (!valid(body)) throw new DiagnosticsError(false, 'invalid');
  return body;
}
