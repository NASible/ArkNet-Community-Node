import { useEffect, useRef, useState } from 'react';
import type { MediaObject } from '../api/media';
import { fetchTransfers, mutateTransfer, TransferError, type TransferAction, type Transfers } from '../api/transfer';
const message = (cause: unknown) => cause instanceof TransferError ? cause.message : 'Cannot reach transfer service. Check the Node Agent and retry.';
const eligible = (object: MediaObject) => object.state === 'ACTIVE' && ['QUEUED', 'ERROR'].includes(object.local_state);
interface Props {
  objects: MediaObject[];
  startAllowed?: boolean;
  autoStart?: { objectId: string; requestId: number };
  onAutoStartSettled?: (objectId: string, error?: string) => void;
  onRealCompletion?: (objectId: string) => void;
}
export default function TransferPanel({ objects, startAllowed = true, autoStart, onAutoStartSettled, onRealCompletion }: Props) {
  const media = useRef({ objects, startAllowed, onAutoStartSettled, onRealCompletion });
  media.current = { objects, startAllowed, onAutoStartSettled, onRealCompletion };
  const [data, setData] = useState<Transfers>();
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const commands = useRef<{ mutate: (id: string, action: TransferAction) => Promise<string | undefined>; retry: () => Promise<void> } | null>(null);
  useEffect(() => {
    let active = true;
    let locked = false;
    let requiresRefresh = false;
    let fresh = false;
    let latest: Transfers | undefined;
    let poll: number | undefined;
    let timeout: number | undefined;
    let controller: AbortController | undefined;
    let refreshing: Promise<boolean> | undefined;
    async function request<T>(operation: (signal: AbortSignal) => Promise<T>) {
      const current = new AbortController();
      controller = current;
      let timedOut = false;
      timeout = window.setTimeout(() => { timedOut = true; current.abort(); }, 10_000);
      try { return await operation(current.signal); }
      catch (cause) { if (timedOut) throw new TransferError('timeout', 'Transfer request timed out after 10 seconds.'); throw cause; }
      finally { window.clearTimeout(timeout); }
    }
    function schedule() {
      window.clearTimeout(poll);
      if (active && !locked && !requiresRefresh) poll = window.setTimeout(() => { void refresh(); }, 1000);
    }
    function refresh(): Promise<boolean> {
      if (refreshing) return refreshing;
      window.clearTimeout(poll);
      refreshing = (async () => {
        try {
          const result = await request(fetchTransfers);
          if (active) {
            latest = result; fresh = true; requiresRefresh = false; setData(result); setError('');
            result.transfers.filter(item => item.state === 'COMPLETED' && !item.mock &&
              media.current.objects.find(object => object.id === item.object_id)?.local_state !== 'AVAILABLE')
              .forEach(item => media.current.onRealCompletion?.(item.object_id));
          }
          return true;
        } catch (cause) {
          fresh = false;
          if (active) setError(message(cause));
          return false;
        } finally { refreshing = undefined; schedule(); }
      })();
      return refreshing;
    }
    async function mutate(id: string, action: TransferAction): Promise<string | undefined> {
      if (!active || locked || requiresRefresh) return 'Transfer controls are busy or require a refresh.';
      locked = true;
      setBusy(true);
      window.clearTimeout(poll);
      try {
        if (refreshing && !await refreshing) return;
        if (!active || !fresh || !latest?.enabled || !latest.available) return 'Transfer service is unavailable.';
        const status = latest.transfers.find(item => item.object_id === id);
        if (action === 'start') {
          const object = media.current.objects.find(item => item.id === id);
          if (status || !object || !eligible(object) || !media.current.startAllowed) return 'Media is not ready to start transferring.';
        } else if (!status || (action === 'pause' && status.state !== 'DOWNLOADING') ||
          (action === 'resume' && status.state !== 'PAUSED') ||
          (action === 'cancel' && !['DOWNLOADING', 'PAUSED'].includes(status.state))) return 'Transfer state changed. Refresh before retrying.';
        await request(signal => mutateTransfer(id, action, signal));
        if (active && !await refresh()) { requiresRefresh = true; return 'Transfer started, but its current status could not be refreshed.'; }
        return undefined;
      } catch (cause) {
        requiresRefresh = true;
        fresh = false;
        if (active) setError(message(cause));
        return message(cause);
      }
      finally { locked = false; if (active) setBusy(false); schedule(); }
    }
    async function retry() {
      if (!active || locked) return;
      locked = true;
      setBusy(true);
      try { await refresh(); }
      finally { locked = false; if (active) setBusy(false); schedule(); }
    }
    commands.current = { mutate, retry };
    void refresh();
    return () => {
      active = false;
      commands.current = null;
      window.clearTimeout(poll);
      window.clearTimeout(timeout);
      controller?.abort();
    };
  }, []);
  useEffect(() => {
    if (!autoStart) return;
    let active = true;
    void commands.current?.mutate(autoStart.objectId, 'start').then(result => {
      if (active) media.current.onAutoStartSettled?.(autoStart.objectId, result);
    });
    return () => { active = false; };
  }, [autoStart?.requestId]);
  const disabled = busy || Boolean(error) || !data?.enabled || !data.available;
  return <section aria-label="Transfers"><h2>Transfers</h2>
    {data && !data.enabled && <p className="notice">Transfer service not configured. No transfer actions are enabled.</p>}
    {data?.enabled && !data.available && <p className="notice">Transfer service unavailable. All transfer controls are disabled.</p>}
    {error && <div role="alert" className="notice error"><p>Transfer progress is stale or unavailable. {error} Refresh transfers before using transfer controls; the last request may have been saved.</p>
      <button type="button" disabled={busy} onClick={() => { void commands.current?.retry(); }}>Refresh transfers</button></div>}
    {data?.transfers.map(status => {
      const object = objects.find(item => item.id === status.object_id);
      const title = object?.title || status.object_id;
      const percent = status.total_bytes === 0 ? (status.state === 'COMPLETED' ? 100 : 0) : Math.round(status.completed_bytes / status.total_bytes * 100);
      const actions: TransferAction[] = status.state === 'DOWNLOADING' ? ['pause', 'cancel'] : status.state === 'PAUSED' ? ['resume', 'cancel'] : [];
      const label = status.mock ? 'Mock transfer' : 'Transfer';
      return <article className="summary-card" key={status.object_id}><h3>{label} — {title}</h3>
        {status.mock && <p>Synthetic progress only — no media is downloaded or verified.</p>}
        <p>{status.state}</p>
        <progress aria-label={`${label} progress for ${title}`} value={percent} max={100} />
        <p>{status.completed_bytes} / {status.total_bytes} bytes ({percent}%)</p>
        {status.state === 'COMPLETED' && <p>{status.mock ? 'Mock complete — no media downloaded'
          : object?.local_state === 'AVAILABLE' ? 'Verified transfer complete — media catalog reports available locally.'
          : 'Verified transfer complete — awaiting catalog confirmation of local availability.'}</p>}
        {status.state === 'VERIFICATION_FAILED' && <p className="notice error">{status.mock
          ? 'Mock verification failed — no media downloaded or verified' : 'Transfer verification failed.'}</p>}
        {status.state === 'CANCELLED' && <p>{status.mock ? 'Mock cancelled — no media downloaded' : 'Transfer cancelled.'}</p>}
        {data.enabled && actions.map(action => <button key={action} type="button" disabled={disabled} onClick={() => { void commands.current?.mutate(status.object_id, action); }}>
          {action[0].toUpperCase() + action.slice(1)} {status.mock ? 'mock transfer' : 'transfer'}</button>)}
      </article>;
    })}
    {data?.enabled && objects.filter(object => eligible(object) && object.id !== autoStart?.objectId && !data.transfers.some(status => status.object_id === object.id)).map(object =>
      <article className="summary-card" key={object.id}><h3>Transfer — {object.title.trim() || 'Untitled media'}</h3>
        <button type="button" disabled={disabled || !startAllowed} onClick={() => { void commands.current?.mutate(object.id, 'start'); }}>Start transfer</button>
      </article>)}
  </section>;
}
