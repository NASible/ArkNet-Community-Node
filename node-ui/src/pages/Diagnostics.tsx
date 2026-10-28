import { useCallback, useEffect, useRef, useState } from 'react';
import { DiagnosticsError, fetchDiagnostics, type Diagnostics as Report } from '../api/diagnostics';
import { AGENT_UNREACHABLE, findings, headline, type Headline, type Tone } from './diagnosticsText';

const REFRESH_MS = 30_000;
const toneLabel: Record<Tone, string> = { healthy: 'OK', info: 'Info', warning: 'Attention', error: 'Problem' };
const toneStatus: Record<Tone, string> = { healthy: 'healthy', info: 'neutral', warning: 'warning', error: 'critical' };

export default function Diagnostics() {
  const [report, setReport] = useState<Report>();
  const [problem, setProblem] = useState<Headline>();
  const [busy, setBusy] = useState(false);
  const mounted = useRef(false);
  const controller = useRef<AbortController | undefined>(undefined);

  const load = useCallback(async () => {
    controller.current?.abort();
    const current = new AbortController();
    controller.current = current;
    setBusy(true);
    try {
      const result = await fetchDiagnostics(current.signal);
      if (mounted.current && !current.signal.aborted) { setReport(result); setProblem(undefined); }
    } catch (cause) {
      if (!mounted.current || current.signal.aborted) return;
      setProblem(cause instanceof DiagnosticsError && !cause.agentUnreachable
        ? { tone: 'error', title: 'Diagnostics unavailable — the Agent returned an unexpected response.', detail: 'Check that the UI and Agent come from the same ArkNet release.' }
        : AGENT_UNREACHABLE);
    } finally { if (mounted.current && !current.signal.aborted) setBusy(false); }
  }, []);

  useEffect(() => {
    mounted.current = true;
    void load();
    const timer = window.setInterval(() => { void load(); }, REFRESH_MS);
    return () => { mounted.current = false; window.clearInterval(timer); controller.current?.abort(); };
  }, [load]);

  const top = problem ?? (report ? headline(report) : undefined);
  return <section aria-labelledby="diagnostics-title">
    <header className="page-heading">
      <div>
        <p className="eyebrow">Troubleshooting</p>
        <h1 id="diagnostics-title">Diagnostics</h1>
        <p className="intro">What is working on this Node, what is not, and what to do about it.</p>
      </div>
      <button type="button" onClick={() => { void load(); }} disabled={busy}>{busy ? 'Checking…' : 'Run checks again'}</button>
    </header>
    {!top && <p role="status" className="footnote">Running checks…</p>}
    {top && <div role={top.tone === 'error' || top.tone === 'warning' ? 'alert' : 'status'} className={`notice diagnostics-headline tone-${top.tone}`}>
      <p><strong>{top.title}</strong></p>
      <p>{top.detail}</p>
    </div>}
    {report && <ul className="diagnostics-list" aria-label="Diagnostic checks">
      {findings(report).map(item => <li key={item.id} className={`detail-panel tone-${item.tone}`} data-check={item.id}>
        <div className="panel-heading">
          <h2>{item.label}</h2>
          <span className="indicator" data-status={toneStatus[item.tone]}><span className="indicator-dot" aria-hidden="true" />{toneLabel[item.tone]}</span>
        </div>
        <p className="diagnostic-summary">{item.summary}</p>
        <p>{item.explanation}</p>
        {item.action && <p className="footnote"><strong>What to do:</strong> {item.action}</p>}
      </li>)}
    </ul>}
    {report && <p className="footnote">Checked <time dateTime={report.generated_at}>{new Date(report.generated_at).toLocaleTimeString()}</time>; refreshes every 30 seconds.</p>}
  </section>;
}
