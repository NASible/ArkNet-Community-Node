import { useEffect, useRef, useState } from 'react';
import { completeLogin, readCallbackAssertion, signOut, startLogin, type MemberSession } from './api/member';

type View =
  | { kind: 'signed-out'; error?: string }
  | { kind: 'working' }
  | { kind: 'signed-in'; session: MemberSession };

// MemberLogin is the global member's sign-in on this Node (/member and the
// Core callback /login/callback). Signing in happens at ArkNet Core.
export default function MemberLogin({ navigate = (url: string) => window.location.assign(url) }: { navigate?: (url: string) => void }) {
  const [view, setView] = useState<View>({ kind: 'signed-out' });
  const handled = useRef(false);

  useEffect(() => {
    if (handled.current) return;
    handled.current = true;
    const assertion = readCallbackAssertion();
    if (assertion === null) return;
    setView({ kind: 'working' });
    completeLogin(assertion)
      .then(session => setView({ kind: 'signed-in', session }))
      .catch((error: Error) => setView({ kind: 'signed-out', error: error.message }));
  }, []);

  async function begin() {
    setView({ kind: 'working' });
    try {
      const started = await startLogin();
      if (started.kind === 'session') setView({ kind: 'signed-in', session: started.session });
      else navigate(started.url);
    } catch (error) {
      setView({ kind: 'signed-out', error: (error as Error).message });
    }
  }

  async function end(session: MemberSession) {
    try { await signOut(session.token); } catch { /* the session ends with this page either way */ }
    setView({ kind: 'signed-out' });
  }

  return (
    <main className="member-login">
      <h1>Member sign-in</h1>
      {view.kind === 'signed-in' ? (
        <section>
          <p>Signed in as member {view.session.member_id.slice(0, 15)}…{view.session.offline ? ' (offline: ArkNet Core is unreachable)' : ''}.</p>
          <p>Session ends {new Date(view.session.expires_at).toLocaleString()}.</p>
          <button type="button" onClick={() => void end(view.session)}>Sign out</button>
        </section>
      ) : (
        <section>
          <p>You sign in at ArkNet Core with your ArkNet account. Your password is never sent to this Node.</p>
          {view.kind === 'signed-out' && view.error && <p role="alert">{view.error}</p>}
          <button type="button" disabled={view.kind === 'working'} onClick={() => void begin()}>Sign in with ArkNet</button>
        </section>
      )}
    </main>
  );
}
