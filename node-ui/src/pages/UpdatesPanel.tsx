import { useEffect, useRef, useState } from 'react';
import { checkForUpdates, fetchUpdates, setAutomaticUpdates, UpdatesError, type UpdateStatus } from '../api/updates';

const reach = 'Cannot reach Node Agent. Check the Agent and local network, then retry.';
const failure = (cause: unknown) => cause instanceof UpdatesError ? cause.message : reach;
const channelLabel = (channel: string | null) => channel === 'stable' ? 'Stable' : channel === 'development' ? 'Development' : 'Unknown';
const formatTime = (value: string) => new Intl.DateTimeFormat('en-US', { year: 'numeric', month: 'short', day: 'numeric', hour: 'numeric', minute: '2-digit', timeZone: 'UTC', timeZoneName: 'short' }).format(new Date(value));

function discoveryMessage(status: UpdateStatus): string {
  switch (status.discovery.state) {
    case 'UPDATE_AVAILABLE': return `ArkNet ${status.available?.version ?? ''} is available on the ${channelLabel(status.channel).toLowerCase()} channel.`;
    case 'UP_TO_DATE': return 'This Node is running the newest release on its channel.';
    case 'NOT_CHECKED': return 'This Node has not checked for updates yet.';
    case 'CHECK_FAILED': return status.available
      ? `The latest check failed; ArkNet ${status.available.version} was found earlier.`
      : 'The update check failed. The release index could not be reached or its release could not be verified. Local services are unaffected.';
    case 'NOT_CONFIGURED': return 'No ArkNet release index is configured for this Node, so updates cannot be discovered.';
    case 'NO_CURRENT_RELEASE': return 'The running release is unknown (no valid release manifest), so updates cannot be compared.';
  }
}

export default function UpdatesPanel() {
  const [status, setStatus] = useState<UpdateStatus>();
  const [busy, setBusy] = useState(true);
  const [error, setError] = useState('');
  const mounted = useRef(false);
  useEffect(() => {
    mounted.current = true;
    const controller = new AbortController();
    void fetchUpdates(controller.signal)
      .then(value => { if (mounted.current) setStatus(value); })
      .catch(cause => { if (mounted.current && !controller.signal.aborted) setError(failure(cause)); })
      .finally(() => { if (mounted.current) setBusy(false); });
    return () => { mounted.current = false; controller.abort(); };
  }, []);

  async function run<T>(action: () => Promise<T>, apply: (value: T) => void) {
    if (busy) return;
    setBusy(true);
    setError('');
    try {
      const value = await action();
      if (mounted.current) apply(value);
    } catch (cause) {
      if (mounted.current) setError(failure(cause));
    } finally { if (mounted.current) setBusy(false); }
  }

  const canCheck = status && status.discovery.state !== 'NOT_CONFIGURED' && status.discovery.state !== 'NO_CURRENT_RELEASE';
  return <section className="detail-panel" aria-labelledby="updates-title">
    <h2 id="updates-title">ArkNet updates</h2>
    {error && <p role="alert" className="notice error">{error}</p>}
    {!status && busy && <p role="status" className="footnote">Loading update status…</p>}
    {status && <>
      <dl className="status-grid">
        <div className="status-row"><dt>Current version</dt><dd>{status.current_version ?? 'Unknown'}</dd></div>
        <div className="status-row"><dt>Available version</dt><dd>{status.available ? status.available.version : 'None'}</dd></div>
        <div className="status-row"><dt>Update channel</dt><dd>{channelLabel(status.channel)}</dd></div>
        <div className="status-row"><dt>Automatic updates</dt><dd>{status.automatic_updates ? 'On' : 'Off'}</dd></div>
        <div className="status-row"><dt>Last checked</dt><dd>{status.discovery.checked_at
          ? <time dateTime={status.discovery.checked_at}>{formatTime(status.discovery.checked_at)}</time> : 'Never'}</dd></div>
      </dl>
      <p role="status">{discoveryMessage(status)}</p>
      {!status.automatic_install_supported && <p className="footnote">
        This ArkNet version discovers updates only; it does not install them yet.
        {status.automatic_updates ? ' With automatic updates on, this Node will install releases from its channel once installation is supported.' : ' Automatic updates are off, so this Node will not install releases automatically when installation is supported.'}
      </p>}
      <div className="setting-panel">
        <label className="switch"><input type="checkbox" aria-label="Automatic updates" checked={status.automatic_updates} disabled={busy}
          onChange={event => { const next = event.currentTarget.checked; void run(() => setAutomaticUpdates(next), enabled => setStatus(s => s && { ...s, automatic_updates: enabled })); }} />
          <span>Automatic updates</span></label>
        <button type="button" disabled={busy || !canCheck} onClick={() => { void run(() => checkForUpdates(), setStatus); }}>Check for updates</button>
      </div>
    </>}
  </section>;
}
