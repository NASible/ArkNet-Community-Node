import { useEffect, useRef, useState } from 'react';
import { fetchPeerServing, SettingsError, updatePeerServing } from '../api/settings';
import UpdatesPanel from './UpdatesPanel';
import { fetchMediaMissPolicy, MediaMissError, updateMediaMissPolicy, type MediaMissPolicy } from '../api/mediaMisses';

const policyLabels: Record<MediaMissPolicy, string> = {
  MANUAL: 'Manual — do not notify',
  ASK: 'Ask — notify the operator',
  AUTOMATIC: 'Automatic — fetch within allocation, one transfer at a time',
};

function MediaMissPolicySetting() {
  const [policy, setPolicy] = useState<MediaMissPolicy>();
  const [busy, setBusy] = useState(true);
  const [error, setError] = useState('');
  const mounted = useRef(false);
  const fail = (cause: unknown) => cause instanceof MediaMissError ? cause.message : 'Cannot reach Node Agent. Check the Agent and local network, then retry.';
  useEffect(() => {
    mounted.current = true;
    const controller = new AbortController();
    void fetchMediaMissPolicy(controller.signal)
      .then(value => { if (mounted.current) setPolicy(value); })
      .catch(cause => { if (mounted.current && !controller.signal.aborted) setError(fail(cause)); })
      .finally(() => { if (mounted.current) setBusy(false); });
    return () => { mounted.current = false; controller.abort(); };
  }, []);
  async function change(next: MediaMissPolicy) {
    if (busy || !policy) return;
    const confirmed = policy;
    setPolicy(next);
    setBusy(true);
    setError('');
    try {
      const saved = await updateMediaMissPolicy(next);
      if (mounted.current) setPolicy(saved);
    } catch (cause) {
      if (mounted.current) { setPolicy(confirmed); setError(fail(cause)); }
    } finally { if (mounted.current) setBusy(false); }
  }
  return <div className="detail-panel setting-panel">
    <div><h2>Missed media</h2><p>What to do when members whose home Node is this one play media from Core because it is not available here.</p>
      {error && <p role="alert" className="notice error">{error}</p>}</div>
    <select aria-label="Missed media policy" value={policy ?? ''} disabled={busy || !policy}
      onChange={event => { void change(event.currentTarget.value as MediaMissPolicy); }}>
      {!policy && <option value="">Loading…</option>}
      {(Object.keys(policyLabels) as MediaMissPolicy[]).map(value => <option key={value} value={value}>{policyLabels[value]}</option>)}
    </select>
  </div>;
}

const message = (error: unknown) => error instanceof SettingsError ? error.message : 'Cannot reach Node Agent. Check the Agent and local network, then retry.';

export default function Settings() {
  const [enabled, setEnabled] = useState(false);
  const [busy, setBusy] = useState(true);
  const [error, setError] = useState('');
  const mounted = useRef(false);

  useEffect(() => {
    mounted.current = true;
    const controller = new AbortController();
    void fetchPeerServing(controller.signal)
      .then(setting => { if (mounted.current) setEnabled(setting.enabled); })
      .catch(cause => { if (mounted.current && !controller.signal.aborted) setError(message(cause)); })
      .finally(() => { if (mounted.current) setBusy(false); });
    return () => { mounted.current = false; controller.abort(); };
  }, []);

  async function change(next: boolean) {
    if (busy) return;
    const confirmed = enabled;
    setEnabled(next);
    setBusy(true);
    setError('');
    try {
      const saved = await updatePeerServing(next);
      if (mounted.current) setEnabled(saved.enabled);
    } catch (cause) {
      if (mounted.current) {
        setEnabled(confirmed);
        setError(message(cause));
      }
    } finally { if (mounted.current) setBusy(false); }
  }

  return <section aria-labelledby="settings-page-title">
    <header className="page-heading"><div><p className="eyebrow">Local management</p><h1 id="settings-page-title">Settings</h1>
      <p className="intro">Control how this independently operated node participates in ArkNet.</p></div></header>
    {error && <div role="alert" className="notice error"><p>{error}</p></div>}
    <div className="detail-panel setting-panel">
      <div><h2>Peer Serving</h2><p>Allow Core to consider this node as a source for ArkNet-managed media it retains.</p>
        <p className="footnote">Receiving content remains available while Peer Serving is off.</p></div>
      <label className="switch"><input type="checkbox" aria-label="Peer Serving" checked={enabled} disabled={busy}
        onChange={event => { void change(event.currentTarget.checked); }} /><span>{enabled ? 'On' : 'Off'}</span></label>
    </div>
    {busy && <p role="status" className="footnote">Saving local setting…</p>}
    <MediaMissPolicySetting />
    <UpdatesPanel />
  </section>;
}
