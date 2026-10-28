import { useEffect, useRef, useState } from 'react';
import { fetchMediaMisses, ignoreMediaMiss, MediaMissError, type MediaMissNotification, type MediaMisses } from '../api/mediaMisses';

const formatBytes = (bytes: number) => {
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB'];
  let value = bytes;
  let unit = 0;
  while (value >= 1024 && unit < units.length - 1) { value /= 1024; unit++; }
  return `${new Intl.NumberFormat('en-US', { maximumFractionDigits: 2 }).format(value)} ${units[unit]}`;
};
const message = (error: unknown) => error instanceof MediaMissError ? error.message : 'Cannot reach Node Agent. Check the Agent and local network, then retry.';

interface Props {
  // Queues through the Media page's normal selection path (allocation-checked).
  onQueue: (notification: MediaMissNotification) => void;
  queueDisabled: boolean;
  // Changes after any media refresh so handled notifications disappear.
  refreshKey: unknown;
}

export default function MissNotifications({ onQueue, queueDisabled, refreshKey }: Props) {
  const [data, setData] = useState<MediaMisses>();
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const [attempt, setAttempt] = useState(0);
  const mounted = useRef(false);
  useEffect(() => {
    mounted.current = true;
    const controller = new AbortController();
    void fetchMediaMisses(controller.signal)
      .then(result => { if (mounted.current) { setData(result); setError(''); } })
      .catch(cause => { if (mounted.current && !controller.signal.aborted) setError(message(cause)); });
    return () => { mounted.current = false; controller.abort(); };
  }, [refreshKey, attempt]);

  async function ignore(id: string) {
    if (busy) return;
    setBusy(true);
    setError('');
    try {
      await ignoreMediaMiss(id);
      if (mounted.current) setAttempt(value => value + 1);
    } catch (cause) {
      if (mounted.current) setError(message(cause));
    } finally { if (mounted.current) setBusy(false); }
  }

  if (error) return <div role="alert" className="notice error"><p>{error}</p></div>;
  if (!data || data.policy === 'MANUAL' || data.notifications.length === 0) return null;
  return <section aria-labelledby="missed-media-title" className="detail-panel">
    <h2 id="missed-media-title">Missed on this Node</h2>
    <p className="footnote">Members whose home Node is this one played these from Core because they are not available here.
      {data.policy === 'AUTOMATIC' ? ' Automatic mode could not fetch these within the storage allocation or transfer policy.' : ''}</p>
    <ul className="media-list">{data.notifications.map(item => <li className="summary-card" key={item.media_id}>
      <h3>{item.title.trim() ? item.title : 'Untitled media'}</h3>
      <p>{formatBytes(item.size)}</p>
      <p>Requested {item.miss_count} {item.miss_count === 1 ? 'time' : 'times'}</p>
      <button type="button" disabled={queueDisabled || busy} onClick={() => onQueue(item)}>Make Available Locally</button>
      <button type="button" disabled={busy} onClick={() => { void ignore(item.media_id); }}>Ignore</button>
    </li>)}</ul>
  </section>;
}
