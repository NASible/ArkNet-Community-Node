export interface AgentStatus {
  node_id: string;
  version: string;
  state: string;
  state_changed_at: string;
  enrollment_state: string;
  core_connectivity: string;
  last_successful_core_contact_at: string | null;
  database_healthy: boolean;
  agent_healthy: boolean;
  jellyfin_healthy: boolean | null;
  // Current whole-stack release from the Node's release manifest; null when
  // the Agent has no valid manifest for its build.
  release: ArkNetRelease | null;
}

export type ReleaseChannel = 'stable' | 'development';
export interface ArkNetRelease { version: string; channel: ReleaseChannel }

const releaseVersion = /^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$/;
const parseRelease = (value: unknown): ArkNetRelease | null | undefined => {
  if (value === undefined || value === null) return null;
  if (typeof value !== 'object' || Array.isArray(value)) return undefined;
  const release = value as Record<string, unknown>;
  if (Object.keys(release).length !== 2 || typeof release.version !== 'string' || !releaseVersion.test(release.version) ||
    (release.channel !== 'stable' && release.channel !== 'development')) return undefined;
  return { version: release.version, channel: release.channel };
};

export type StatusFetchErrorKind =
  | 'agent-unavailable'
  | 'database-unavailable'
  | 'invalid-payload'
  | 'http-error';

export class StatusFetchError extends Error {
  constructor(public readonly kind: StatusFetchErrorKind, message: string) {
    super(message);
    this.name = 'StatusFetchError';
  }
}

const nonemptyString = (value: unknown): value is string =>
  typeof value === 'string' && value.trim().length > 0;

const rfc3339 = (value: unknown): value is string => {
  if (!nonemptyString(value)) return false;
  const shape = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})$/;
  return shape.test(value) && !Number.isNaN(Date.parse(value));
};

const invalidPayload = () => new StatusFetchError(
  'invalid-payload',
  'Agent returned an invalid status response.',
);

export async function fetchAgentStatus(signal: AbortSignal): Promise<AgentStatus> {
  const response = await fetch('/api/v1/status', { signal, cache: 'no-store' });

  if (!response.ok) {
    if (response.status === 502) {
      throw new StatusFetchError('agent-unavailable', 'The Node Agent did not respond through the UI proxy.');
    }
    if (response.status === 503) {
      throw new StatusFetchError('database-unavailable', 'The local state database is unavailable.');
    }
    throw new StatusFetchError('http-error', `The status endpoint returned HTTP ${response.status}.`);
  }

  let data: unknown;
  try {
    data = await response.json();
  } catch {
    throw invalidPayload();
  }

  if (typeof data !== 'object' || data === null || Array.isArray(data)) throw invalidPayload();
  const status = data as Record<string, unknown>;
  const release = parseRelease(status.release);

  if (
    !nonemptyString(status.node_id) ||
    !nonemptyString(status.version) ||
    !nonemptyString(status.state) ||
    !rfc3339(status.state_changed_at) ||
    !nonemptyString(status.enrollment_state) ||
    !nonemptyString(status.core_connectivity) ||
    !(status.last_successful_core_contact_at === null || rfc3339(status.last_successful_core_contact_at)) ||
    typeof status.database_healthy !== 'boolean' ||
    typeof status.agent_healthy !== 'boolean' ||
    release === undefined
  ) {
    throw invalidPayload();
  }

  return {
    node_id: status.node_id,
    version: status.version,
    state: status.state,
    state_changed_at: status.state_changed_at,
    enrollment_state: status.enrollment_state,
    core_connectivity: status.core_connectivity,
    last_successful_core_contact_at: status.last_successful_core_contact_at,
    database_healthy: status.database_healthy,
    agent_healthy: status.agent_healthy,
    jellyfin_healthy: typeof status.jellyfin_healthy === 'boolean' ? status.jellyfin_healthy : null,
    release,
  };
}
