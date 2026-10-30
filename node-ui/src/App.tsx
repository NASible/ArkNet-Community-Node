import { useCallback, useEffect, useState, type FormEvent } from 'react';
import Console from './Console';
import {
  beginSetup, confirmSetup, currentSession, signIn, signOut, OperatorError,
  type OperatorSession, type SetupStart,
} from './api/operator';

// Re-check the session regularly so an expired or revoked session returns
// to the sign-in screen instead of failing page by page.
const SESSION_CHECK_MS = 60_000;

type View =
  | { kind: 'loading' }
  | { kind: 'error'; message: string }
  | { kind: 'sign-in'; notice?: string }
  | { kind: 'setup' }
  | { kind: 'recovery-codes'; session: OperatorSession; codes: string[] }
  | { kind: 'console'; session: OperatorSession };

const message = (error: unknown) => error instanceof OperatorError ? error.message : 'The Node Agent is not responding. Check the Node and retry.';

function Header({ session, onSignOut }: { session?: OperatorSession; onSignOut?: () => void }) {
  return (
    <header className="app-header"><div className="container"><strong>ArkNet</strong><span>Community Node</span>
      {session && <span className="operator">Signed in as {session.username}
        <button type="button" onClick={onSignOut}>Sign out</button></span>}
    </div></header>
  );
}

function SignIn({ notice, onSignedIn, onSetup }: { notice?: string; onSignedIn: (s: OperatorSession) => void; onSetup: () => void }) {
  const [username, setUsername] = useState('');
  const [password, setPassword] = useState('');
  const [code, setCode] = useState('');
  const [useRecovery, setUseRecovery] = useState(false);
  const [error, setError] = useState(notice ?? '');
  const [busy, setBusy] = useState(false);
  const submit = async (event: FormEvent) => {
    event.preventDefault();
    setBusy(true); setError('');
    try {
      onSignedIn(await signIn(username, password, useRecovery ? { recovery_code: code } : { totp: code }));
    } catch (e) { setError(message(e)); setPassword(''); setCode(''); }
    finally { setBusy(false); }
  };
  return (
    <section className="card" aria-labelledby="signin-heading">
      <h1 id="signin-heading">Operator sign-in</h1>
      <form onSubmit={submit}>
        <label>Username<input autoComplete="username" value={username} onChange={e => setUsername(e.target.value)} required /></label>
        <label>Password<input type="password" autoComplete="current-password" value={password} onChange={e => setPassword(e.target.value)} required /></label>
        <label>{useRecovery ? 'Recovery code' : 'Authenticator code'}
          <input autoComplete="one-time-code" inputMode={useRecovery ? 'text' : 'numeric'} value={code} onChange={e => setCode(e.target.value.trim())} required /></label>
        <button type="button" className="link" onClick={() => { setUseRecovery(!useRecovery); setCode(''); }}>
          {useRecovery ? 'Use the authenticator app instead' : 'Use a recovery code instead'}</button>
        {error && <p role="alert">{error}</p>}
        <button type="submit" disabled={busy}>Sign in</button>
      </form>
      <p className="hint">Lost access? On the Node, run <code>sudo arknet-node operator recover NAME</code> and choose
        {' '}<button type="button" className="link" onClick={onSetup}>Set up operator</button> with the printed code.</p>
    </section>
  );
}

function Setup({ onDone, onCancel }: { onDone: (s: OperatorSession, codes: string[]) => void; onCancel?: () => void }) {
  const [setupCode, setSetupCode] = useState('');
  const [username, setUsername] = useState('');
  const [password, setPassword] = useState('');
  const [started, setStarted] = useState<SetupStart | null>(null);
  const [totp, setTotp] = useState('');
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const start = async (event: FormEvent) => {
    event.preventDefault();
    setBusy(true); setError('');
    try { setStarted(await beginSetup(setupCode, username, password)); setPassword(''); setSetupCode(''); }
    catch (e) { setError(message(e)); }
    finally { setBusy(false); }
  };
  const confirm = async (event: FormEvent) => {
    event.preventDefault();
    if (!started) return;
    setBusy(true); setError('');
    try { const done = await confirmSetup(started.enrollment_token, totp); onDone(done.session, done.recoveryCodes); }
    catch (e) { setError(message(e)); setTotp(''); }
    finally { setBusy(false); }
  };
  if (started) {
    return (
      <section className="card" aria-labelledby="setup-heading">
        <h1 id="setup-heading">Add your authenticator</h1>
        <p>Add this account to an authenticator app (for example Aegis, 2FAS or a password manager), then enter the current 6-digit code.</p>
        <p><a href={started.totp_uri}>Open in authenticator app</a></p>
        <p>Or enter this key manually: <code aria-label="Authenticator key">{started.totp_secret}</code></p>
        <form onSubmit={confirm}>
          <label>Authenticator code<input autoComplete="one-time-code" inputMode="numeric" value={totp} onChange={e => setTotp(e.target.value.trim())} required /></label>
          {error && <p role="alert">{error}</p>}
          <button type="submit" disabled={busy}>Finish setup</button>
        </form>
      </section>
    );
  }
  return (
    <section className="card" aria-labelledby="setup-heading">
      <h1 id="setup-heading">Set up operator</h1>
      <p>On the Node, run <code>sudo arknet-node operator setup-code</code> (or <code>operator recover NAME</code>) and enter the printed code.</p>
      <form onSubmit={start}>
        <label>Setup or recovery code<input autoComplete="off" value={setupCode} onChange={e => setSetupCode(e.target.value)} required /></label>
        <label>Username<input autoComplete="username" value={username} onChange={e => setUsername(e.target.value)} required /></label>
        <label>New password<input type="password" autoComplete="new-password" value={password} onChange={e => setPassword(e.target.value)} required minLength={12} /></label>
        {error && <p role="alert">{error}</p>}
        <button type="submit" disabled={busy}>Continue</button>
        {onCancel && <button type="button" className="link" onClick={onCancel}>Back to sign-in</button>}
      </form>
    </section>
  );
}

function RecoveryCodes({ codes, onContinue }: { codes: string[]; onContinue: () => void }) {
  return (
    <section className="card" aria-labelledby="recovery-heading">
      <h1 id="recovery-heading">Save your recovery codes</h1>
      <p>Each code signs you in once without the authenticator. They are shown only now; store them somewhere safe.</p>
      <ul aria-label="Recovery codes">{codes.map(code => <li key={code}><code>{code}</code></li>)}</ul>
      <button type="button" onClick={onContinue}>I saved them</button>
    </section>
  );
}

export default function App() {
  const [view, setView] = useState<View>({ kind: 'loading' });
  const refresh = useCallback(async (notice?: string) => {
    try {
      const state = await currentSession();
      setView(state.kind === 'signed-in' ? { kind: 'console', session: state.session }
        : state.kind === 'setup-required' ? { kind: 'setup' } : { kind: 'sign-in', notice });
    } catch (e) { setView({ kind: 'error', message: message(e) }); }
  }, []);
  useEffect(() => { void refresh(); }, [refresh]);
  const signedIn = view.kind === 'console';
  useEffect(() => {
    if (!signedIn) return;
    const timer = setInterval(() => {
      void currentSession().then(state => {
        if (state.kind !== 'signed-in') setView({ kind: 'sign-in', notice: 'Your session ended. Sign in again.' });
      }).catch(() => undefined);
    }, SESSION_CHECK_MS);
    return () => clearInterval(timer);
  }, [signedIn]);

  const doSignOut = async () => {
    try { await signOut(); } finally { setView({ kind: 'sign-in', notice: 'Signed out.' }); }
  };
  if (view.kind === 'console') {
    return <><Header session={view.session} onSignOut={() => void doSignOut()} /><Console /></>;
  }
  return (
    <>
      <Header />
      <main className="container">
        {view.kind === 'loading' && <p>Loading…</p>}
        {view.kind === 'error' && <p role="alert">{view.message} <button type="button" onClick={() => void refresh()}>Retry</button></p>}
        {view.kind === 'sign-in' && <SignIn notice={view.notice} onSignedIn={session => setView({ kind: 'console', session })} onSetup={() => setView({ kind: 'setup' })} />}
        {view.kind === 'setup' && <Setup onDone={(session, codes) => setView({ kind: 'recovery-codes', session, codes })} onCancel={() => setView({ kind: 'sign-in' })} />}
        {view.kind === 'recovery-codes' && <RecoveryCodes codes={view.codes} onContinue={() => setView({ kind: 'console', session: view.session })} />}
      </main>
    </>
  );
}
