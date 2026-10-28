import { useEffect, useRef, useState } from 'react';
import { fetchMedia, selectMedia, deleteRetainedMedia, MediaError, type LocalState, type MediaObject, type MediaCatalog } from '../api/media';
import TransferPanel from './TransferPanel';
import MissNotifications from './MissNotifications';

const formatBytes = (bytes: number) => {
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB'];
  let value = bytes;
  let unit = 0;
  while (value >= 1024 && unit < units.length - 1) { value /= 1024; unit++; }
  return `${new Intl.NumberFormat('en-US', { maximumFractionDigits: 2 }).format(value)} ${units[unit]}`;
};
const localLabels: Record<LocalState, string> = {
  NOT_PRESENT: 'Not present', QUEUED: 'Queued', DOWNLOADING: 'Downloading', AVAILABLE: 'Available',
  REMOVED_RETAINED: 'Removed — retained locally', ERROR: 'Error',
};
const errorMessage = (error: unknown) => error instanceof MediaError ? error.message
  : 'Cannot reach Node Agent. Check the Agent and local network, then retry.';
export default function Media() {
  const [catalog, setCatalog] = useState<MediaCatalog>();
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(true);
  const [attempt, setAttempt] = useState(0);
  const request = useRef<{ controller: AbortController; timer: number } | null>(null);
  const mounted = useRef(false);
  const actionLocked = useRef(false);
  const completionRefreshLocked = useRef(false);
  const [confirmed, setConfirmed] = useState<Record<string, MediaObject>>({});
  const [autoStart, setAutoStart] = useState<{ objectId: string; requestId: number }>();
  const [transferActionError, setTransferActionError] = useState('');
  async function runRequest<T>(operation: (signal: AbortSignal) => Promise<T>): Promise<T> {
    const controller = new AbortController();
    let timedOut = false;
    const timer = window.setTimeout(() => { timedOut = true; controller.abort(); }, 10_000);
    request.current = { controller, timer };
    try { return await operation(controller.signal); }
    catch (cause) { if (timedOut) throw new MediaError('timeout', 'Media request timed out after 10 seconds. Check the Agent and retry.'); throw cause; }
    finally { window.clearTimeout(timer); if (request.current?.controller === controller) request.current = null; }
  }
  useEffect(() => {
    mounted.current = true;
    let active = true;
    setBusy(true);
    setError('');
    void runRequest(fetchMedia).then(data => { if (active) setCatalog(data); })
      .catch((cause: unknown) => { if (active) setError(errorMessage(cause)); })
      .finally(() => { if (active) setBusy(false); });
    return () => {
      active = false;
      mounted.current = false;
      request.current?.controller.abort();
      window.clearTimeout(request.current?.timer);
    };
  }, [attempt]);
  async function queue(object: MediaObject) {
    if (actionLocked.current || busy || error || !catalog || !catalog.core_available || catalog.allocation_bytes === 0 ||
      (catalog.allocation_bytes !== -1 && object.size > catalog.allocation_bytes - catalog.reserved_bytes) || object.state !== 'ACTIVE' || object.local_state !== 'NOT_PRESENT') return;
    actionLocked.current = true;
    setBusy(true);
    setError('');
    setTransferActionError('');
    let selected = false;
    try {
      const queued = await runRequest(signal => selectMedia(object.id, signal));
      if (!mounted.current) return;
      selected = true;
      setConfirmed(current => ({ ...current, [object.id]: queued }));
      const refreshed = await runRequest(fetchMedia);
      if (mounted.current) setCatalog(refreshed);
    } catch (cause) {
      if (mounted.current) setError(selected
        ? `${errorMessage(cause)} The selection was saved, but media status could not be refreshed. Refresh media before making another selection.`
        : `${errorMessage(cause)} Refresh media before making another selection; the previous request may have been saved.`);
    } finally {
      actionLocked.current = false;
      if (mounted.current) {
        setBusy(false);
        if (selected) setAutoStart(current => ({ objectId: object.id, requestId: (current?.requestId ?? 0) + 1 }));
      }
    }
  }
  async function deleteRetained(object: MediaObject) {
    if (actionLocked.current || busy || object.state !== 'REMOVED' || object.local_state !== 'REMOVED_RETAINED') return;
    actionLocked.current = true;
    setBusy(true);
    setError('');
    try {
      await runRequest(signal => deleteRetainedMedia(object.id, signal));
      const refreshed = await runRequest(fetchMedia);
      if (mounted.current) setCatalog(refreshed);
    } catch (cause) {
      if (mounted.current) setError(errorMessage(cause));
    } finally {
      actionLocked.current = false;
      if (mounted.current) setBusy(false);
    }
  }
  function autoStartSettled(objectId: string, transferError?: string) {
    setAutoStart(current => current?.objectId === objectId ? undefined : current);
    setTransferActionError(transferError ? `Media was queued, but the transfer did not start. ${transferError}` : '');
  }
  async function refreshCompletedMedia() {
    if (completionRefreshLocked.current || request.current) return;
    completionRefreshLocked.current = true;
    try {
      const refreshed = await runRequest(fetchMedia);
      if (mounted.current) setCatalog(refreshed);
    } catch (cause) {
      if (mounted.current) setError(`${errorMessage(cause)} The transfer completed, but local availability could not be refreshed.`);
    } finally { completionRefreshLocked.current = false; }
  }
  const displayedObjects = catalog?.objects.map(item => item.state === 'ACTIVE' && item.local_state === 'NOT_PRESENT' && Object.hasOwn(confirmed, item.id)
    ? { ...item, local_state: confirmed[item.id].local_state } : item) ?? [];
  return <section aria-labelledby="media-page-title">
    <header className="page-heading"><div><p className="eyebrow">Local management</p><h1 id="media-page-title">Media</h1>
      <p className="intro">Make media available with one action. Progress and controls appear below.</p></div></header>
    <p className="footnote">Selections queue only; transfers require an explicit start, which this action sends after selection succeeds.</p>
    {busy && <div role="status" className="notice loading">Loading media…</div>}
    {error && <div role="alert" className="notice error"><p>{error}</p>
      <button type="button" disabled={busy} onClick={() => setAttempt(value => value + 1)}>Retry media refresh</button></div>}
    {transferActionError && <div role="alert" className="notice error"><p>{transferActionError}</p></div>}
    {catalog && <MissNotifications refreshKey={catalog}
      queueDisabled={busy || Boolean(error) || !catalog.core_available || catalog.allocation_bytes === 0}
      onQueue={item => { void queue({ id: item.media_id, title: item.title, size: item.size, state: 'ACTIVE', local_state: 'NOT_PRESENT' }); }} />}
    {catalog && <>
      {!catalog.core_available && <p className="notice">Core is offline. Showing saved selections; new selections are disabled.</p>}
      <dl className="status-grid detail-panel"><div className="status-row"><dt>Allocation</dt><dd>{catalog.allocation_bytes === -1 ? 'Unlimited' : formatBytes(catalog.allocation_bytes)}</dd></div>
        <div className="status-row"><dt>Reserved</dt><dd>{formatBytes(catalog.reserved_bytes)}</dd></div></dl>
      <p className="footnote">The Agent enforces the storage allocation. Configure a positive allocation in GiB or -1 for unlimited before selecting media.</p>
      {catalog.objects.length === 0 && <p className="notice">No media is available in this catalog.</p>}
      <ul className="media-list">{displayedObjects.map(object => {
        return <li className="summary-card" key={object.id}>
        <h2>{object.title.trim() ? object.title : 'Untitled media'}</h2><p>{formatBytes(object.size)}</p><p>{localLabels[object.local_state]}</p>
        {object.state === 'REMOVED' && <p>Removed from catalog</p>}
        {object.state === 'REMOVED' && object.local_state === 'REMOVED_RETAINED' && <button type="button"
          onClick={() => { void deleteRetained(object); }} disabled={busy}>Delete retained local copy</button>}
        {object.state === 'ACTIVE' && object.local_state === 'NOT_PRESENT' && <button type="button"
          onClick={() => { void queue(object); }}
          disabled={busy || Boolean(error) || !catalog.core_available || catalog.allocation_bytes === 0 || (catalog.allocation_bytes !== -1 && object.size > catalog.allocation_bytes - catalog.reserved_bytes)}>
          Make Available Locally</button>}
      </li>; })}</ul>
    </>}
    <TransferPanel objects={displayedObjects} startAllowed={!busy && Boolean(catalog?.core_available)} autoStart={autoStart}
      onAutoStartSettled={autoStartSettled} onRealCompletion={() => { void refreshCompletedMedia(); }} />
  </section>;
}
