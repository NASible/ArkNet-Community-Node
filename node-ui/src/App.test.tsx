import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import App from './App';

const session = { operator_id: 'op_' + 'a'.repeat(64), username: 'alice', authenticated_at: '2026-10-28T00:00:00Z',
  idle_expires_at: '2026-10-28T00:30:00Z', expires_at: '2026-10-28T12:00:00Z' };
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
let storageWrites: unknown[];

beforeEach(() => {
  storageWrites = [];
  vi.spyOn(Storage.prototype, 'setItem').mockImplementation((...args) => { storageWrites.push(args); });
});
afterEach(() => { cleanup(); vi.unstubAllGlobals(); vi.restoreAllMocks(); });

function stubFetch(routes: Record<string, (init?: RequestInit) => Response>) {
  const fetchMock = vi.fn(async (url: string, init?: RequestInit) => {
    const key = `${init?.method ?? 'GET'} ${url}`;
    const handler = routes[key];
    return handler ? handler(init) : new Promise<Response>(() => {});
  });
  vi.stubGlobal('fetch', fetchMock);
  return fetchMock;
}

it('signs in with password and authenticator code, then signs out', async () => {
  let signedIn = false;
  const fetchMock = stubFetch({
    'GET /api/v1/operator/session': () => signedIn ? json(200, session) : json(401, { error: 'operator_required' }),
    'POST /api/v1/operator/sessions': init => {
      const body = JSON.parse(String(init?.body));
      if (body.password !== 'correct horse operator' || body.totp !== '123456') return json(401, { error: 'invalid_credentials' });
      signedIn = true; return json(201, session);
    },
    'DELETE /api/v1/operator/session': () => { signedIn = false; return new Response(null, { status: 204 }); },
  });
  render(<App />);
  expect(await screen.findByRole('heading', { name: 'Operator sign-in' })).toBeTruthy();
  fireEvent.change(screen.getByLabelText('Username'), { target: { value: 'alice' } });
  fireEvent.change(screen.getByLabelText('Password'), { target: { value: 'wrong password!!' } });
  fireEvent.change(screen.getByLabelText('Authenticator code'), { target: { value: '123456' } });
  fireEvent.click(screen.getByRole('button', { name: 'Sign in' }));
  expect((await screen.findByRole('alert')).textContent).toContain('Sign-in failed');
  fireEvent.change(screen.getByLabelText('Password'), { target: { value: 'correct horse operator' } });
  fireEvent.change(screen.getByLabelText('Authenticator code'), { target: { value: '123456' } });
  fireEvent.click(screen.getByRole('button', { name: 'Sign in' }));
  expect(await screen.findByText('Signed in as alice')).toBeTruthy();
  expect(screen.getByRole('heading', { name: 'Node dashboard' })).toBeTruthy();
  fireEvent.click(screen.getByRole('button', { name: 'Sign out' }));
  expect(await screen.findByRole('heading', { name: 'Operator sign-in' })).toBeTruthy();
  expect(fetchMock.mock.calls.some(([url, init]) => url === '/api/v1/operator/session' && init?.method === 'DELETE')).toBe(true);
  // Credentials always go in the body; the UI never sets an Authorization header.
  expect(fetchMock.mock.calls.every(([, init]) => !JSON.stringify(init?.headers ?? {}).includes('Authorization'))).toBe(true);
  expect(storageWrites).toEqual([]);
  expect(document.cookie).toBe('');
});

it('can sign in with a recovery code instead of the authenticator', async () => {
  let sent: Record<string, string> = {};
  stubFetch({
    'GET /api/v1/operator/session': () => json(401, { error: 'operator_required' }),
    'POST /api/v1/operator/sessions': init => { sent = JSON.parse(String(init?.body)); return json(201, session); },
  });
  render(<App />);
  await screen.findByRole('heading', { name: 'Operator sign-in' });
  fireEvent.click(screen.getByRole('button', { name: 'Use a recovery code instead' }));
  fireEvent.change(screen.getByLabelText('Username'), { target: { value: 'alice' } });
  fireEvent.change(screen.getByLabelText('Password'), { target: { value: 'correct horse operator' } });
  fireEvent.change(screen.getByLabelText('Recovery code'), { target: { value: 'ABCDE-FGHIJ' } });
  fireEvent.click(screen.getByRole('button', { name: 'Sign in' }));
  expect(await screen.findByText('Signed in as alice')).toBeTruthy();
  expect(sent).toEqual({ username: 'alice', password: 'correct horse operator', recovery_code: 'ABCDE-FGHIJ' });
});

it('walks through first-operator setup and shows recovery codes once', async () => {
  const codes = Array.from({ length: 10 }, (_, i) => `AAAA${i}-BBBBB`);
  stubFetch({
    'GET /api/v1/operator/session': () => json(401, { error: 'operator_required' }),
    'POST /api/v1/operator/setup': () => json(201, { enrollment_token: 'enroll-token', purpose: 'SETUP', username: 'alice',
      totp_uri: 'otpauth://totp/ArkNet%20Node:alice?secret=JBSWY3DPEHPK3PXP', totp_secret: 'JBSWY3DPEHPK3PXP', expires_at: '2026-10-28T00:15:00Z' }),
    'POST /api/v1/operator/setup/confirm': init => {
      expect(JSON.parse(String(init?.body))).toEqual({ enrollment_token: 'enroll-token', totp: '654321' });
      return json(201, { session, recovery_codes: codes });
    },
  });
  render(<App />);
  await screen.findByRole('heading', { name: 'Operator sign-in' });
  fireEvent.click(screen.getByRole('button', { name: 'Set up operator' }));
  expect(await screen.findByRole('heading', { name: 'Set up operator' })).toBeTruthy();
  fireEvent.change(screen.getByLabelText('Setup or recovery code'), { target: { value: 'ABCD-EFGH' } });
  fireEvent.change(screen.getByLabelText('Username'), { target: { value: 'alice' } });
  fireEvent.change(screen.getByLabelText('New password'), { target: { value: 'correct horse operator' } });
  fireEvent.click(screen.getByRole('button', { name: 'Continue' }));
  expect(await screen.findByLabelText('Authenticator key')).toBeTruthy();
  fireEvent.change(screen.getByLabelText('Authenticator code'), { target: { value: '654321' } });
  fireEvent.click(screen.getByRole('button', { name: 'Finish setup' }));
  const list = await screen.findByRole('list', { name: 'Recovery codes' });
  expect(list.querySelectorAll('li')).toHaveLength(10);
  fireEvent.click(screen.getByRole('button', { name: 'I saved them' }));
  expect(await screen.findByText('Signed in as alice')).toBeTruthy();
  expect(screen.queryByRole('list', { name: 'Recovery codes' })).toBeNull();
  expect(storageWrites).toEqual([]);
});

it('returns to sign-in when the session ends while the console is open', async () => {
  vi.useFakeTimers({ shouldAdvanceTime: true });
  let valid = true;
  stubFetch({ 'GET /api/v1/operator/session': () => valid ? json(200, session) : json(401, { error: 'operator_required' }) });
  render(<App />);
  expect(await screen.findByText('Signed in as alice')).toBeTruthy();
  valid = false;
  await vi.advanceTimersByTimeAsync(61_000);
  await waitFor(() => expect(screen.getByRole('heading', { name: 'Operator sign-in' })).toBeTruthy());
  expect(screen.getByRole('alert').textContent).toContain('session ended');
  vi.useRealTimers();
});
