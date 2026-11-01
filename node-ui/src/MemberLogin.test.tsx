import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import MemberLogin from './MemberLogin';

const memberId = 'member_' + 'b'.repeat(64);
const session = { member_id: memberId, kind: 'GLOBAL', permissions: ['MEDIA_ACCESS'], offline: false, token: 'T'.repeat(43), expires_at: '2026-10-31T12:00:00Z' };
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
let storageWrites: unknown[];

beforeEach(() => {
  storageWrites = [];
  vi.spyOn(Storage.prototype, 'setItem').mockImplementation((...args) => { storageWrites.push(args); });
  window.history.replaceState(null, '', '/member');
});
afterEach(() => { cleanup(); vi.unstubAllGlobals(); vi.restoreAllMocks(); });

function stubFetch(routes: Record<string, (init?: RequestInit) => Response>) {
  const fetchMock = vi.fn(async (url: string, init?: RequestInit) => {
    const handler = routes[`${init?.method ?? 'GET'} ${url}`];
    return handler ? handler(init) : json(404, { error: 'not_found' });
  });
  vi.stubGlobal('fetch', fetchMock);
  return fetchMock;
}

it('starts sign-in at Core and never asks for the global password', async () => {
  let sent: Record<string, string> = {};
  stubFetch({ 'POST /api/v1/global-users/login': init => {
    sent = JSON.parse(String(init?.body));
    return json(200, { mode: 'redirect', authorize_url: 'https://core.example/node-login?node_id=x' });
  } });
  const navigate = vi.fn();
  render(<MemberLogin navigate={navigate} />);
  expect(await screen.findByRole('heading', { name: 'Member sign-in' })).toBeTruthy();
  expect(document.querySelector('input[type="password"]')).toBeNull();
  fireEvent.click(screen.getByRole('button', { name: 'Sign in with ArkNet' }));
  await vi.waitFor(() => expect(navigate).toHaveBeenCalledWith('https://core.example/node-login?node_id=x'));
  expect(sent).toEqual({ callback_url: `${window.location.origin}/login/callback` });
});

it('refuses to navigate anywhere but an http(s) Core URL', async () => {
  stubFetch({ 'POST /api/v1/global-users/login': () => json(200, { mode: 'redirect', authorize_url: 'javascript:alert(1)' }) });
  const navigate = vi.fn();
  render(<MemberLogin navigate={navigate} />);
  fireEvent.click(await screen.findByRole('button', { name: 'Sign in with ArkNet' }));
  expect((await screen.findByRole('alert')).textContent).toContain('invalid');
  expect(navigate).not.toHaveBeenCalled();
});

it('completes the callback from the URL fragment, clears it and keeps nothing in storage', async () => {
  window.history.replaceState(null, '', '/login/callback#assertion=PAYLOAD.SIGNATURE');
  let sent: Record<string, string> = {};
  const fetchMock = stubFetch({ 'POST /api/v1/global-users/login/complete': init => {
    sent = JSON.parse(String(init?.body));
    return json(201, session);
  } });
  render(<MemberLogin navigate={vi.fn()} />);
  expect(await screen.findByText(/Signed in as member member_bbbbbbbb/)).toBeTruthy();
  expect(sent).toEqual({ assertion: 'PAYLOAD.SIGNATURE' });
  expect(window.location.hash).toBe('');
  expect(window.location.pathname).toBe('/member');
  expect(storageWrites).toEqual([]);
  // Sign-out uses the bearer token held in memory only.
  fetchMock.mockImplementation(async (url: string, init?: RequestInit) =>
    url === '/api/v1/global-users/session' && init?.method === 'DELETE' ? new Response(null, { status: 204 }) : json(404, {}));
  fireEvent.click(screen.getByRole('button', { name: 'Sign out' }));
  expect(await screen.findByRole('button', { name: 'Sign in with ArkNet' })).toBeTruthy();
  const [, init] = fetchMock.mock.calls.at(-1)!;
  expect(JSON.stringify(init?.headers)).toContain('Bearer ' + session.token);
});

it('shows an offline session when Core is unreachable', async () => {
  stubFetch({ 'POST /api/v1/global-users/login': () => json(201, { ...session, mode: 'session', offline: true }) });
  const navigate = vi.fn();
  render(<MemberLogin navigate={navigate} />);
  fireEvent.click(await screen.findByRole('button', { name: 'Sign in with ArkNet' }));
  expect(await screen.findByText(/offline/i)).toBeTruthy();
  expect(navigate).not.toHaveBeenCalled();
});

it('explains a failed or replayed callback', async () => {
  window.history.replaceState(null, '', '/login/callback#assertion=REPLAYED.X');
  stubFetch({ 'POST /api/v1/global-users/login/complete': () => json(401, { error: 'invalid_global_login' }) });
  render(<MemberLogin navigate={vi.fn()} />);
  expect((await screen.findByRole('alert')).textContent).toContain('Start again');
  expect(window.location.hash).toBe('');
});

it('explains a rejected offline login', async () => {
  stubFetch({ 'POST /api/v1/global-users/login': () => json(401, { error: 'offline_login_rejected' }) });
  render(<MemberLogin navigate={vi.fn()} />);
  fireEvent.click(await screen.findByRole('button', { name: 'Sign in with ArkNet' }));
  expect((await screen.findByRole('alert')).textContent).toContain('online');
});
