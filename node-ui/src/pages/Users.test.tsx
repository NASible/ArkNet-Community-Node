import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, expect, it, vi } from 'vitest';
import Users from './Users';

afterEach(() => { cleanup(); vi.unstubAllGlobals(); });

const localID = 'local_' + 'a'.repeat(64);
const alice = { user_id: localID, username: 'alice', kind: 'LOCAL', status: 'ACTIVE', created_at: '2026-10-25T00:00:00Z' };
const json = (value: unknown, status = 200) => new Response(JSON.stringify(value), { status, headers: { 'Content-Type': 'application/json' } });

function route(handlers: Record<string, () => Response>) {
  const fetchMock = vi.fn(async (path: string, init?: RequestInit) => {
    const handler = handlers[`${init?.method ?? 'GET'} ${path}`];
    if (!handler) throw new Error(`unexpected ${init?.method ?? 'GET'} ${path}`);
    return handler();
  });
  vi.stubGlobal('fetch', fetchMock);
  return fetchMock;
}

it('loads and renders local users only', async () => {
  const fetchMock = route({
    'GET /api/v1/local-users': () => json({ users: [alice], truncated: false }),
  });
  render(<Users />);
  const locals = await screen.findByRole('list', { name: 'Local users' });
  expect(within(locals).getByText('alice')).toBeTruthy();
  expect(within(locals).getByText('Local')).toBeTruthy();
  expect(screen.queryByRole('list', { name: 'Global members' })).toBeNull();
  expect(screen.queryByText('ArkNet global members')).toBeNull();
  expect(fetchMock).toHaveBeenCalledTimes(1);
  expect(fetchMock).toHaveBeenCalledWith('/api/v1/local-users', expect.anything());
  expect(screen.getByText(/never sent to Core/)).toBeTruthy();
});

it('keeps local users usable without requesting Core-backed members', async () => {
  let users = [alice];
  const fetchMock = route({
    'GET /api/v1/local-users': () => json({ users, truncated: false }),
    'POST /api/v1/local-users': () => {
      const bob = { ...alice, user_id: 'local_' + 'c'.repeat(64), username: 'bob' };
      users = [alice, bob];
      return json(bob, 201);
    },
  });
  render(<Users />);
  expect(await screen.findByText('alice')).toBeTruthy();
  fireEvent.change(screen.getByLabelText('Username'), { target: { value: 'bob' } });
  fireEvent.change(screen.getByLabelText('Password'), { target: { value: 'correct horse battery' } });
  fireEvent.click(screen.getByRole('button', { name: 'Create local user' }));
  expect(await screen.findByText('bob')).toBeTruthy();
  expect(fetchMock).toHaveBeenCalledWith('/api/v1/local-users', expect.objectContaining({
    method: 'POST', body: '{"username":"bob","password":"correct horse battery"}',
  }));
});

it('shows the Agent error and disables a local user', async () => {
  let current = alice;
  route({
    'GET /api/v1/local-users': () => json({ users: [current], truncated: false }),
    'POST /api/v1/local-users': () => json({ error: 'username_taken' }, 409),
    [`POST /api/v1/local-users/${localID}/disable`]: () => { current = { ...alice, status: 'DISABLED' }; return json(current); },
  });
  render(<Users />);
  fireEvent.click(await screen.findByRole('button', { name: 'Disable alice' }));
  expect(await screen.findByRole('button', { name: 'Enable alice' })).toBeTruthy();
  fireEvent.change(screen.getByLabelText('Username'), { target: { value: 'alice' } });
  fireEvent.change(screen.getByLabelText('Password'), { target: { value: 'correct horse battery' } });
  fireEvent.click(screen.getByRole('button', { name: 'Create local user' }));
  await waitFor(() => expect(screen.getByRole('alert').textContent).toContain('already exists on this node'));
});
