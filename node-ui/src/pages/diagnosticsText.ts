// Human explanations for Agent diagnostics. Keep these plain-language and
// actionable: say what is affected, what still works, and what to do.
import type { Diagnostics, ProbeStatus } from '../api/diagnostics';

export type Tone = 'healthy' | 'info' | 'warning' | 'error';
export interface Finding { id: string; label: string; tone: Tone; summary: string; explanation: string; action?: string }
export interface Headline { tone: Tone; title: string; detail: string }

export const formatBytes = (bytes: number) => {
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB'];
  let value = bytes;
  let unit = 0;
  while (value >= 1024 && unit < units.length - 1) { value /= 1024; unit++; }
  return `${new Intl.NumberFormat('en-US', { maximumFractionDigits: 1 }).format(value)} ${units[unit]}`;
};

const ago = (iso: string, now: Date) => {
  const seconds = Math.max(0, Math.round((now.getTime() - Date.parse(iso)) / 1000));
  if (seconds < 90) return `${seconds} seconds ago`;
  if (seconds < 5400) return `${Math.round(seconds / 60)} minutes ago`;
  if (seconds < 172800) return `${Math.round(seconds / 3600)} hours ago`;
  return `${Math.round(seconds / 86400)} days ago`;
};

function networkProblem(status: ProbeStatus, what: string, host: string, httpStatus?: number): { summary: string; explanation: string; action: string } {
  switch (status) {
    case 'dns': return { summary: 'Address not found', explanation: `The address of ${what} (${host}) could not be found.`,
      action: 'Check the address configured for this Node and that this machine has a working internet connection and DNS.' };
    case 'refused': return { summary: 'Connection refused', explanation: `${host} answered but refused the connection, so ${what} is not accepting connections right now.`,
      action: 'It may be down for maintenance. If this persists, check that the configured address and port are correct.' };
    case 'timeout': return { summary: 'No response', explanation: `${host} did not respond. This machine may be offline, or a firewall is blocking outbound connections.`,
      action: 'Check this machine\'s network connection and any firewall that controls outbound HTTPS.' };
    case 'unreachable': return { summary: 'No network route', explanation: `This machine has no network route to ${host}.`,
      action: 'Check this machine\'s network cable, Wi-Fi or router.' };
    case 'tls': return { summary: 'Secure connection rejected', explanation: `The secure connection to ${host} failed because its certificate could not be verified.`,
      action: 'Check that this machine\'s clock is correct and that the configured address is the real ArkNet service.' };
    case 'server_error': return { summary: 'Service reported a problem', explanation: `${what} is reachable but reported an internal problem${httpStatus ? ` (HTTP ${httpStatus})` : ''}. The problem is on the ${what} side, not this Node.`,
      action: 'No action is needed on this Node; it reconnects automatically when the service recovers.' };
    default: return { summary: 'Unreachable', explanation: `${what} at ${host} could not be reached.`, action: 'Check this machine\'s network connection.' };
  }
}

const NODE_STATE_TEXT: Record<string, string> = {
  ENROLLMENT_PENDING: 'Waiting for an ArkNet Core administrator to approve this Node.',
  CONNECTED: 'Connected to ArkNet Core.',
  SYNCHRONIZING: 'Connected and synchronizing with ArkNet.',
  MESH: 'Connected to the ArkNet mesh.',
  DEGRADED: 'Recent contact with Core failed; retrying.',
  OFFLINE: 'Out of contact with Core; retrying automatically.',
  RECONNECTING: 'Reconnecting to Core.',
};

export function findings(d: Diagnostics, now = new Date()): Finding[] {
  const out: Finding[] = [];
  const release = d.version.release;
  out.push({ id: 'version', label: 'ArkNet version', tone: release ? 'healthy' : 'warning',
    summary: release ? `ArkNet ${release.version} (${release.channel})` : `Agent build ${d.version.agent}`,
    explanation: release ? 'This Node is running a verified ArkNet release.' : 'The release manifest is missing or does not match this Agent, so the running release cannot be confirmed.',
    action: release ? undefined : 'Re-run the installer or `arknet-node upgrade` to restore the release files.' });

  out.push({ id: 'agent', label: 'ArkNet Agent', tone: 'healthy', summary: 'Running', explanation: 'The Node Agent is answering.' });

  out.push(d.database.status === 'healthy'
    ? { id: 'database', label: 'PostgreSQL', tone: 'healthy', summary: 'Healthy', explanation: 'The Node\'s local database is available.' }
    : { id: 'database', label: 'PostgreSQL', tone: 'error', summary: 'Unavailable',
      explanation: 'The Agent cannot reach the Node\'s local database, so settings, users and media selections cannot be read or saved.',
      action: 'Restart the Node stack (re-run the installer). If it persists, the disk may be full or the database container stopped.' });

  out.push(d.jellyfin.status === 'healthy'
    ? { id: 'jellyfin', label: 'Jellyfin', tone: 'healthy', summary: 'Healthy', explanation: 'The media server is running; members can stream from this Node.' }
    : d.jellyfin.status === 'not_configured'
      ? { id: 'jellyfin', label: 'Jellyfin', tone: 'info', summary: 'Not monitored', explanation: 'This Agent is not configured to check Jellyfin.' }
      : { id: 'jellyfin', label: 'Jellyfin', tone: 'error', summary: 'Not responding',
        explanation: 'The media server is not responding, so members cannot stream from this Node. Federation and media selections are unaffected.',
        action: 'Restart the Node stack (re-run the installer). If Jellyfin keeps failing, check free disk space.' });

  const core = d.core;
  if (core.status === 'not_configured') {
    out.push({ id: 'core', label: 'Core connectivity', tone: 'info', summary: 'Not configured',
      explanation: 'This Node is not connected to an ArkNet Core, so it cannot join the federation. Local services work normally.',
      action: 'Re-install with --core-url to enroll this Node.' });
  } else if (core.node_state === 'REVOKED' || core.node_state === 'QUARANTINED') {
    out.push({ id: 'core', label: 'Core connectivity', tone: 'error', summary: core.node_state === 'REVOKED' ? 'Access revoked' : 'Quarantined',
      explanation: `ArkNet Core has ${core.node_state === 'REVOKED' ? 'revoked' : 'quarantined'} this Node. It no longer receives federation access. Local services keep running.`,
      action: 'Contact your ArkNet Core administrator.' });
  } else if (core.status !== 'ok') {
    const p = networkProblem(core.status, 'ArkNet Core', core.host ?? 'Core', core.http_status);
    const contact = core.last_contact_at ? ` Last successful contact: ${ago(core.last_contact_at, now)}.`
      : core.node_state === 'ENROLLMENT_PENDING' ? ' Enrollment was requested earlier; no heartbeat has been recorded yet.'
        : ' No successful heartbeat with Core has been recorded yet.';
    out.push({ id: 'core', label: 'Core connectivity', tone: 'error', summary: `Federation offline — ${p.summary.toLowerCase()}`,
      explanation: `${p.explanation}${contact} Local services keep running, and the Node reconnects automatically.`, action: p.action });
  } else if (core.node_state === 'UNKNOWN') {
    out.push({ id: 'core', label: 'Core connectivity', tone: 'warning', summary: 'Reachable; Node state unknown',
      explanation: `${core.host} is reachable, but this Node's federation state cannot be read because the local database is unavailable.` });
  } else {
    const pending = core.node_state === 'ENROLLMENT_PENDING';
    const retrying = ['DEGRADED', 'OFFLINE', 'RECONNECTING'].includes(core.node_state);
    out.push({ id: 'core', label: 'Core connectivity', tone: pending ? 'info' : retrying ? 'warning' : 'healthy',
      summary: pending ? 'Waiting for approval' : retrying ? 'Reconnecting' : 'Connected',
      explanation: `${core.host} is reachable. ${NODE_STATE_TEXT[core.node_state] ?? `Node state: ${core.node_state}.`}` +
        (core.last_contact_at ? ` Last successful contact: ${ago(core.last_contact_at, now)}.` : ''),
      action: pending ? 'Ask your ArkNet Core administrator to approve this Node.' : undefined });
  }

  if (d.tracker.status === 'not_configured') {
    out.push({ id: 'tracker', label: 'Private tracker', tone: 'info', summary: 'Not configured',
      explanation: 'Transfers are not enabled on this Node, so it does not use the ArkNet private tracker.' });
  } else if (d.tracker.status === 'ok') {
    out.push({ id: 'tracker', label: 'Private tracker', tone: 'healthy', summary: 'Reachable', explanation: `${d.tracker.host} is reachable for authorized swarms.` });
  } else {
    const p = networkProblem(d.tracker.status, 'the ArkNet private tracker', d.tracker.host ?? 'the tracker');
    out.push({ id: 'tracker', label: 'Private tracker', tone: 'warning', summary: p.summary,
      explanation: `${p.explanation} Downloads cannot find peer Nodes; they continue from Core's web seed while Core is reachable.`, action: p.action });
  }

  out.push(d.tailscale.status === 'detected'
    ? { id: 'tailscale', label: 'Tailscale', tone: 'info', summary: 'Detected', explanation: 'Tailscale is running on this machine. This ArkNet release does not depend on it.' }
    : { id: 'tailscale', label: 'Tailscale', tone: 'info', summary: 'Not detected',
      explanation: 'Tailscale is not running on this machine. That is fine: this ArkNet release reaches Core directly over outbound HTTPS.' });

  if (d.storage.status !== 'healthy') {
    out.push({ id: 'storage', label: 'Storage capacity', tone: 'warning', summary: 'Unknown', explanation: 'The Agent could not read the storage capacity.' });
  } else {
    const share = d.storage.free_bytes / d.storage.total_bytes;
    const where = d.storage.kind === 'media' ? 'the media volume' : 'Docker\'s data disk';
    out.push({ id: 'storage', label: 'Storage capacity', tone: share < 0.05 ? 'error' : share < 0.1 ? 'warning' : 'healthy',
      summary: `${formatBytes(d.storage.free_bytes)} free of ${formatBytes(d.storage.total_bytes)}`,
      explanation: share < 0.1 ? `${where} is almost full. Databases, Jellyfin and downloads fail when it runs out.` : `Free space on ${where}.`,
      action: share < 0.1 ? 'Free disk space or enlarge the disk, and lower the ArkNet media allocation if needed.' : undefined });
  }

  if (d.media.status !== 'healthy') {
    out.push({ id: 'media', label: 'ArkNet media storage', tone: 'warning', summary: 'Unknown', explanation: 'Media selections could not be read from the local database.' });
  } else if (d.media.allocation_bytes === 0) {
    out.push({ id: 'media', label: 'ArkNet media storage', tone: 'info', summary: 'Disabled (0 GiB allocated)',
      explanation: 'No storage is allocated to ArkNet media, so nothing can be made available locally.',
      action: 'Set ARKNET_MEDIA_STORAGE_ALLOCATION_GIB to allocate storage.' });
  } else {
    const limit = d.media.allocation_bytes === -1 ? 'unlimited' : formatBytes(d.media.allocation_bytes);
    const full = d.media.allocation_bytes > 0 && d.media.reserved_bytes >= d.media.allocation_bytes * 0.95;
    out.push({ id: 'media', label: 'ArkNet media storage', tone: full ? 'warning' : 'healthy',
      summary: `${formatBytes(d.media.reserved_bytes)} of ${limit} used`,
      explanation: `${d.media.selections} ${d.media.selections === 1 ? 'title is' : 'titles are'} stored or reserved for this Node.${full ? ' The allocation is nearly full; new selections will be refused.' : ''}` });
  }

  const t = d.transfers;
  if (t.status === 'not_configured') {
    out.push({ id: 'transfers', label: 'Active transfers', tone: 'info', summary: 'Transfers not enabled', explanation: 'The transfer service is not part of this Node\'s installation.' });
  } else if (t.status === 'unavailable') {
    out.push({ id: 'transfers', label: 'Active transfers', tone: 'error', summary: 'Transfer service not responding',
      explanation: 'Downloads cannot start, pause or finish until the transfer service responds again.', action: 'Restart the Node stack (re-run the installer).' });
  } else {
    out.push({ id: 'transfers', label: 'Active transfers', tone: t.failed > 0 ? 'warning' : 'healthy',
      summary: `${t.active} downloading, ${t.paused} paused${t.failed ? `, ${t.failed} failed` : ''}`,
      explanation: t.failed ? 'Some transfers failed. Open the Media page to retry or cancel them.' : `${t.completed} completed.` });
  }
  return out;
}

export function headline(d: Diagnostics): Headline {
  const local = d.database.status === 'healthy' && d.jellyfin.status !== 'unhealthy';
  const federationDown = d.core.status !== 'ok' && d.core.status !== 'not_configured';
  if (d.database.status !== 'healthy') {
    return { tone: 'error', title: 'Local Database Unavailable — Node settings cannot be read or saved.', detail: 'Jellyfin may still stream already-available media.' };
  }
  if (d.jellyfin.status === 'unhealthy') {
    return { tone: 'error', title: `Media Server Offline — members cannot stream from this Node.`,
      detail: federationDown ? 'ArkNet Core is also unreachable.' : 'Federation and the Agent are still operating.' };
  }
  if (federationDown && local) {
    return { tone: 'warning', title: 'Federation Offline — Local services are still operating.', detail: 'Members can keep streaming media already on this Node; the Node reconnects to ArkNet Core automatically.' };
  }
  if (d.core.node_state === 'REVOKED' || d.core.node_state === 'QUARANTINED') {
    return { tone: 'error', title: 'Node Access Revoked — this Node no longer participates in ArkNet.', detail: 'Local services keep running.' };
  }
  const problems = findings(d).filter(f => f.tone === 'error' || f.tone === 'warning');
  if (problems.length) {
    return { tone: 'warning', title: `${problems.length} ${problems.length === 1 ? 'item needs' : 'items need'} attention.`, detail: problems.map(p => p.label).join(', ') + '.' };
  }
  if (d.core.node_state === 'ENROLLMENT_PENDING') {
    return { tone: 'info', title: 'Waiting for Approval — ArkNet Core has not approved this Node yet.', detail: 'Local services are operating normally.' };
  }
  return { tone: 'healthy', title: 'All Systems Operational.', detail: 'Every service on this Node is working.' };
}

export const AGENT_UNREACHABLE: Headline = {
  tone: 'error',
  title: 'Node Agent Offline — the management UI cannot reach the Agent.',
  detail: 'Jellyfin may still be streaming. Restart the Node stack by re-running the installer; if that fails, check the machine\'s free disk space.',
};
