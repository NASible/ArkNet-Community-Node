import { FormEvent, useCallback, useEffect, useRef, useState } from 'react';
import { createLocalUser, fetchLocalUsers, LocalUser, setLocalUserEnabled, UsersError } from '../api/users';

const message = (error: unknown) => error instanceof UsersError ? error.message : 'Cannot reach Node Agent. Check the Agent and local network, then retry.';

export default function Users() {
  const [local, setLocal] = useState<LocalUser[]>([]);
  const [username, setUsername] = useState('');
  const [password, setPassword] = useState('');
  const [busy, setBusy] = useState(true);
  const [error, setError] = useState('');
  const mounted = useRef(false);

  const load = useCallback(async (signal?: AbortSignal) => {
    // Local users come from this node only and load even when Core is offline.
    try {
      const localResult = await fetchLocalUsers(signal);
      if (mounted.current && !signal?.aborted) setLocal(localResult);
    } catch (cause) {
      if (mounted.current && !signal?.aborted) setError(message(cause));
    }
  }, []);

  useEffect(() => {
    mounted.current = true;
    const controller = new AbortController();
    void load(controller.signal).finally(() => { if (mounted.current) setBusy(false); });
    return () => { mounted.current = false; controller.abort(); };
  }, [load]);

  async function run(action: () => Promise<unknown>) {
    setBusy(true);
    setError('');
    try { await action(); await load(); } catch (cause) { if (mounted.current) setError(message(cause)); }
    finally { if (mounted.current) setBusy(false); }
  }

  function submit(event: FormEvent) {
    event.preventDefault();
    void run(async () => { await createLocalUser(username, password); setUsername(''); setPassword(''); });
  }

  return <section aria-labelledby="users-page-title">
    <header className="page-heading"><div><p className="eyebrow">Local management</p><h1 id="users-page-title">Users</h1>
      <p className="intro">Manage accounts that exist only on this node.</p></div></header>
    {error && <div role="alert" className="notice error"><p>{error}</p></div>}

    <div className="detail-panel users-panel" aria-labelledby="local-users-title">
      <div className="panel-heading"><h2 id="local-users-title">Local users</h2><span className="badge badge-local">Local only</span></div>
      <p className="footnote panel-note">Stored only on this node and never sent to Core. They work while Core is offline and are not valid on other nodes.
        The same username on another node, or in ArkNet, is a different account.</p>
      <form className="user-form" onSubmit={submit}>
        <label>Username<input name="username" autoComplete="off" value={username} onChange={event => setUsername(event.currentTarget.value)} required /></label>
        <label>Password<input name="password" type="password" autoComplete="new-password" value={password} onChange={event => setPassword(event.currentTarget.value)} required /></label>
        <button type="submit" disabled={busy}>Create local user</button>
      </form>
      {local.length === 0 ? <p className="footnote panel-note">No local users yet.</p> :
        <ul className="user-list" aria-label="Local users">{local.map(user => <li key={user.user_id} className="user-row">
          <span className="badge badge-local">Local</span><strong>{user.username}</strong>
          <span className="detail">{user.status === 'ACTIVE' ? 'Active' : 'Disabled'}</span>
          <button type="button" disabled={busy} onClick={() => { void run(() => setLocalUserEnabled(user.user_id, user.status !== 'ACTIVE')); }}>
            {user.status === 'ACTIVE' ? `Disable ${user.username}` : `Enable ${user.username}`}</button>
        </li>)}</ul>}
    </div>
    {busy && <p role="status" className="footnote">Working…</p>}
  </section>;
}
