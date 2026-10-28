import { useEffect, useState } from 'react';
import { fetchAgentStatus, StatusFetchError, type AgentStatus } from '../api/status';

type ErrorKind = 'agent-unavailable' | 'database-unavailable' | 'invalid-payload' | 'timeout' | 'network' | 'other';
type StatusState =
  | { kind: 'loading' }
  | { kind: 'ready'; status: AgentStatus; refreshError?: ErrorKind }
  | { kind: 'error'; errorKind: ErrorKind };

type Tone = 'healthy' | 'warning' | 'critical' | 'neutral';
type DisplayValue = { label: string; tone: Tone };

const nodeStates: Record<string, DisplayValue> = {
  PROVISIONING: { label: 'Provisioning', tone: 'warning' },
  ENROLLMENT_PENDING: { label: 'Enrollment pending', tone: 'warning' },
  CONNECTED: { label: 'Connected', tone: 'healthy' },
  SYNCHRONIZING: { label: 'Synchronizing', tone: 'warning' },
  MESH: { label: 'Mesh connected', tone: 'healthy' },
  DEGRADED: { label: 'Degraded', tone: 'warning' },
  OFFLINE: { label: 'Offline', tone: 'critical' },
  RECONNECTING: { label: 'Reconnecting', tone: 'warning' },
  QUARANTINED: { label: 'Quarantined', tone: 'critical' },
  REVOKED: { label: 'Revoked', tone: 'critical' },
  ERROR: { label: 'Error', tone: 'critical' },
};

const enrollmentStates: Record<string, DisplayValue> = {
  NOT_ENROLLED: { label: 'Not enrolled', tone: 'neutral' },
  PENDING: { label: 'Pending approval', tone: 'warning' },
  APPROVED: { label: 'Approved', tone: 'healthy' },
  REJECTED: { label: 'Rejected', tone: 'critical' },
};

const coreStates: Record<string, DisplayValue> = {
  UNKNOWN: { label: 'Unknown', tone: 'neutral' },
  CONNECTED: { label: 'Connected', tone: 'healthy' },
  DEGRADED: { label: 'Degraded', tone: 'warning' },
  OFFLINE: { label: 'Offline', tone: 'critical' },
  RECONNECTING: { label: 'Reconnecting', tone: 'warning' },
  NOT_IMPLEMENTED: { label: 'Not implemented', tone: 'neutral' },
};

const displayMachineValue = (value: string, known: Record<string, DisplayValue>): DisplayValue =>
  known[value] ?? { label: `Unknown (${value})`, tone: 'neutral' };

const formatTimestamp = (value: string) => new Intl.DateTimeFormat('en-US', {
  month: 'short',
  day: 'numeric',
  year: 'numeric',
  hour: 'numeric',
  minute: '2-digit',
  second: '2-digit',
  timeZone: 'UTC',
  timeZoneName: 'short',
}).format(new Date(value));

function Indicator({ value, machineValue }: { value: DisplayValue; machineValue?: string }) {
  return (
    <span className="indicator" data-status={value.tone} data-machine-value={machineValue}>
      <span className="indicator-dot" aria-hidden="true" />
      {value.label}
    </span>
  );
}

const errorCopy: Record<ErrorKind, { title: string; guidance: string }> = {
  'agent-unavailable': {
    title: 'Node Agent is not responding',
    guidance: 'Check that the Agent process is running and that the UI proxy target points to the local Agent, then retry.',
  },
  'database-unavailable': {
    title: 'Local database unavailable',
    guidance: 'Check that PostgreSQL is running and that the Agent can reach its configured database, then retry.',
  },
  'invalid-payload': {
    title: 'Status response is incompatible',
    guidance: 'Check that the UI and Agent versions are compatible. The response was not used because required status fields were missing or invalid.',
  },
  timeout: {
    title: 'Status request timed out',
    guidance: 'The Agent did not answer within 10 seconds. Check whether the Agent is running or overloaded, then retry.',
  },
  network: {
    title: 'Cannot reach Node Agent',
    guidance: 'Check that the Agent is running, the local network is available, and the UI proxy target is correct, then retry.',
  },
  other: {
    title: 'Status check failed',
    guidance: 'Check the Node Agent logs and the UI proxy target, then retry the status check.',
  },
};

function classifyError(error: unknown, timedOut: boolean): ErrorKind {
  if (timedOut) return 'timeout';
  if (error instanceof StatusFetchError) return error.kind === 'http-error' ? 'other' : error.kind;
  if (error instanceof TypeError) return 'network';
  return 'other';
}

function JellyfinIndicator({
  healthy,
  agentHealthy,
}: {
  healthy: boolean | null;
  agentHealthy: boolean;
}) {
  if (!agentHealthy)
    return <Indicator value={{ label: 'Agent unhealthy', tone: 'warning' }} />;
  if (healthy === null)
    return <Indicator value={{ label: 'Not reported', tone: 'neutral' }} />;
  if (healthy)
    return <Indicator value={{ label: 'Healthy', tone: 'healthy' }} />;
  return <Indicator value={{ label: 'Unhealthy', tone: 'critical' }} />;
}

function jellyfinMessage(
  healthy: boolean | null,
  agentHealthy: boolean,
): string {
  if (!agentHealthy)
    return 'Node Agent health check failed; Jellyfin status may be stale.';
  if (healthy === null) return 'Jellyfin configured but its health probe is not reported by the Agent.';
  if (healthy)
    return 'Jellyfin is reachable and healthy.';
  return 'Jellyfin is configured but its health probe is not responding.';
}

export default function Dashboard() {
  const [result, setResult] = useState<StatusState>({ kind: 'loading' });
  const [attempt, setAttempt] = useState(0);

  useEffect(() => {
    let active = true;
    let controller: AbortController | undefined;
    let requestTimer: number | undefined;
    let refreshTimer: number | undefined;

    const loadStatus = (showLoading: boolean) => {
      controller = new AbortController();
      const requestController = controller;
      let timedOut = false;
      if (showLoading) setResult({ kind: 'loading' });
      requestTimer = window.setTimeout(() => {
        timedOut = true;
        requestController.abort();
      }, 10_000);

      fetchAgentStatus(requestController.signal)
        .then((status) => { if (active) setResult({ kind: 'ready', status }); })
        .catch((error: unknown) => {
          if (!active) return;
          const errorKind = classifyError(error, timedOut);
          setResult((current) => current.kind === 'ready'
            ? { ...current, refreshError: errorKind }
            : { kind: 'error', errorKind });
        })
        .finally(() => {
          window.clearTimeout(requestTimer);
          if (active) refreshTimer = window.setTimeout(() => loadStatus(false), 5_000);
        });
    };

    loadStatus(true);
    return () => {
      active = false;
      window.clearTimeout(requestTimer);
      window.clearTimeout(refreshTimer);
      controller?.abort();
    };
  }, [attempt]);

  return (
    <section aria-labelledby="dashboard-title">
      <header className="page-heading">
        <div>
          <p className="eyebrow">Local management · Read only</p>
          <h1 id="dashboard-title">Node dashboard</h1>
          <p className="intro">Operational status reported by this node's Agent.</p>
        </div>
        <span className="live-label"><span aria-hidden="true" />Auto-refresh · 5s</span>
      </header>

      {result.kind === 'loading' && (
        <div className="notice loading" role="status">Loading Node Agent status…</div>
      )}

      {result.kind === 'error' && (
        <div className="notice error" role="alert">
          <p className="notice-kicker">Status unavailable</p>
          <h2>{errorCopy[result.errorKind].title}</h2>
          <p>{errorCopy[result.errorKind].guidance}</p>
          <p className="detail">Endpoint: <code>/api/v1/status</code></p>
          <button type="button" onClick={() => setAttempt((value) => value + 1)}>Retry status check</button>
        </div>
      )}

      {result.kind === 'ready' && (
        <>
          {result.refreshError && (
            <div className="notice error refresh-warning" role="alert">
              <p className="notice-kicker">Background refresh failed</p>
              <h2>{errorCopy[result.refreshError].title}</h2>
              <p>{errorCopy[result.refreshError].guidance} The last successful status remains below.</p>
              <button type="button" onClick={() => setAttempt((value) => value + 1)}>Retry status check</button>
            </div>
          )}
          <StatusView status={result.status} />
        </>
      )}
    </section>
  );
}

function StatusView({ status }: { status: AgentStatus }) {
  const nodeState = displayMachineValue(status.state, nodeStates);
  const enrollment = displayMachineValue(status.enrollment_state, enrollmentStates);
  const core = displayMachineValue(status.core_connectivity, coreStates);
  const localHealthy = status.agent_healthy && status.database_healthy;
  const localSummary: DisplayValue = localHealthy
    ? { label: 'Running', tone: 'healthy' }
    : { label: 'Needs attention', tone: 'critical' };
  const federationCopy = status.core_connectivity === 'OFFLINE'
    ? 'Federation is offline; local services are still available.'
    : status.core_connectivity === 'CONNECTED'
      ? 'This node is communicating with ArkNet Core.'
      : 'Core communication is not fully available. Local status remains accessible.';

  return (
    <>
      <div className="summary-grid" aria-label="Node status summary">
        <section className="summary-card" aria-labelledby="local-services-title">
          <div className="summary-heading">
            <p className="eyebrow">This machine</p>
            <h2 id="local-services-title">Local services</h2>
          </div>
          <Indicator value={localSummary} />
          <p>{localHealthy ? 'Agent and database checks are healthy.' : 'One or more local service checks require attention.'}</p>
        </section>
        <section className="summary-card" aria-labelledby="federation-title">
          <div className="summary-heading">
            <p className="eyebrow">Remote</p>
            <h2 id="federation-title">Federation</h2>
          </div>
          <Indicator value={core} />
          <p>{federationCopy}</p>
        </section>
        <section className="summary-card" aria-labelledby="media-title">
          <div className="summary-heading">
            <p className="eyebrow">Media</p>
            <h2 id="media-title">Jellyfin</h2>
          </div>
          <JellyfinIndicator
            healthy={status.jellyfin_healthy}
            agentHealthy={status.agent_healthy}
          />
          <p>
            {jellyfinMessage(status.jellyfin_healthy, status.agent_healthy)}
          </p>
        </section>
      </div>

      <section className="detail-panel" aria-labelledby="node-details-title">
        <div className="panel-heading">
          <div>
            <p className="eyebrow">Status details</p>
            <h2 id="node-details-title">Node identity and health</h2>
          </div>
          <span className="version">ArkNet {status.release?.version ?? status.version}</span>
        </div>
        <dl className="status-grid">
          <div className="status-row status-row-wide">
            <dt>Node ID</dt>
            <dd><code className="node-id">{status.node_id}</code></dd>
          </div>
          <div className="status-row"><dt>ArkNet version</dt><dd>{status.release?.version ?? status.version}</dd></div>
          <div className="status-row"><dt>Release channel</dt><dd>{status.release
            ? (status.release.channel === 'stable' ? 'Stable' : 'Development')
            : `Release manifest unavailable — showing Agent build ${status.version}`}</dd></div>
          <div className="status-row"><dt>Node state</dt><dd><Indicator value={nodeState} machineValue={status.state} /></dd></div>
          <div className="status-row"><dt>State changed</dt><dd><time dateTime={status.state_changed_at}>{formatTimestamp(status.state_changed_at)}</time></dd></div>
          <div className="status-row"><dt>Enrollment</dt><dd><Indicator value={enrollment} machineValue={status.enrollment_state} /></dd></div>
          <div className="status-row"><dt>Core connectivity</dt><dd><Indicator value={core} machineValue={status.core_connectivity} /></dd></div>
          <div className="status-row"><dt>Last successful Core contact</dt><dd>{status.last_successful_core_contact_at === null
            ? 'Never'
            : <time dateTime={status.last_successful_core_contact_at}>{formatTimestamp(status.last_successful_core_contact_at)}</time>}
          </dd></div>
          <div className="status-row"><dt>Database health</dt><dd><Indicator value={status.database_healthy
            ? { label: 'Healthy', tone: 'healthy' }
            : { label: 'Unhealthy', tone: 'critical' }} /></dd></div>
          <div className="status-row"><dt>Agent health</dt><dd><Indicator value={status.agent_healthy
            ? { label: 'Healthy', tone: 'healthy' }
            : { label: 'Unhealthy', tone: 'critical' }} /></dd></div>
          <div className="status-row"><dt>Jellyfin health</dt><dd><JellyfinIndicator healthy={status.jellyfin_healthy} agentHealthy={status.agent_healthy} /></dd></div>
        </dl>
      </section>

      <p className="footnote">Read-only status. No credentials, enrollment actions, or configuration controls are exposed here.</p>
    </>
  );
}
