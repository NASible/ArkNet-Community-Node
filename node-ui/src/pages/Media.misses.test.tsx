import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, expect, it, vi } from 'vitest';
import Media from './Media';
import Settings from './Settings';

const missed = `media_${'a'.repeat(64)}`;
const other = `media_${'b'.repeat(64)}`;
const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
afterEach(() => { cleanup(); vi.unstubAllGlobals(); });
// The panel loads after the catalog; allow for a loaded CI/test runner.
const slow = { timeout: 5000 };

// A small fake Agent routed by URL so request ordering does not matter.
function fakeAgent(policy = 'ASK') {
  const state = { selected: new Set<string>(), ignored: new Set<string>(), policy, calls: [] as string[] };
  const titles: Record<string, string> = { [missed]: 'Missed film', [other]: 'Other film' };
  const fetchMock = vi.fn(async (url: string, options: RequestInit = {}) => {
    const method = options.method ?? 'GET';
    state.calls.push(`${method} ${url}`);
    if (url === '/api/v1/media') return json({
      objects: [missed, other].map(id => ({ id, title: titles[id], size: 1024, state: 'ACTIVE', local_state: state.selected.has(id) ? 'QUEUED' : 'NOT_PRESENT' })),
      allocation_bytes: 1 << 30, reserved_bytes: state.selected.size * 1024, core_available: true,
    });
    if (url === '/api/v1/media-misses') return json({
      policy: state.policy,
      notifications: state.policy === 'MANUAL' ? [] : [missed].filter(id => !state.selected.has(id) && !state.ignored.has(id))
        .map(id => ({ media_id: id, title: titles[id], size: 1024, miss_count: 3 })),
    });
    const selection = url.match(/^\/api\/v1\/media\/(media_[0-9a-f]{64})\/selection$/);
    if (selection && method === 'POST') {
      state.selected.add(selection[1]);
      return json({ id: selection[1], title: titles[selection[1]], size: 1024, state: 'ACTIVE', local_state: 'QUEUED' });
    }
    const ignore = url.match(/^\/api\/v1\/media-misses\/(media_[0-9a-f]{64})\/ignore$/);
    if (ignore && method === 'POST') { state.ignored.add(ignore[1]); return new Response(null, { status: 204 }); }
    if (url === '/api/v1/transfers') return json({ enabled: false, available: false, transfers: [] });
    if (url === '/api/v1/settings/media-miss-policy') {
      if (method === 'PUT') state.policy = JSON.parse(String(options.body)).policy;
      return json({ policy: state.policy });
    }
    if (url === '/api/v1/settings/peer-serving') return json({ enabled: false });
    return json({ error: 'not_found' }, 404);
  });
  vi.stubGlobal('fetch', fetchMock);
  return state;
}

it('notifies the operator in ASK mode and queues missed media from the notification', async () => {
  const agent = fakeAgent();
  render(<Media />);
  const panel = (await screen.findByRole('heading', { name: 'Missed on this Node' }, slow)).closest('section') as HTMLElement;
  expect(within(panel).getByText('Missed film')).toBeTruthy();
  expect(within(panel).getByText('Requested 3 times')).toBeTruthy();
  expect(within(panel).queryByText('Other film')).toBeNull();
  fireEvent.click(within(panel).getByRole('button', { name: 'Make Available Locally' }));
  await waitFor(() => expect(agent.calls).toContain(`POST /api/v1/media/${missed}/selection`), slow);
  // The notification disappears once the selection exists, and the catalog shows it queued.
  await waitFor(() => expect(screen.queryByRole('heading', { name: 'Missed on this Node' })).toBeNull(), slow);
  expect(await screen.findByText('Queued', undefined, slow)).toBeTruthy();
  expect(agent.calls.filter(call => call.startsWith('POST')).length).toBe(1);
});

it('lets the operator ignore a missed-media notification', async () => {
  const agent = fakeAgent();
  render(<Media />);
  const panel = (await screen.findByRole('heading', { name: 'Missed on this Node' }, slow)).closest('section') as HTMLElement;
  fireEvent.click(within(panel).getByRole('button', { name: 'Ignore' }));
  await waitFor(() => expect(screen.queryByRole('heading', { name: 'Missed on this Node' })).toBeNull(), slow);
  expect(agent.calls).toContain(`POST /api/v1/media-misses/${missed}/ignore`);
  expect(agent.selected.size).toBe(0);
});

it('shows no notifications in MANUAL mode', async () => {
  fakeAgent('MANUAL');
  render(<Media />);
  expect(await screen.findByText('Missed film', undefined, slow)).toBeTruthy();
  await waitFor(() => expect(screen.queryByRole('heading', { name: 'Missed on this Node' })).toBeNull(), slow);
});

it('shows ASK as the default missed-media policy and saves an explicit change', async () => {
  const agent = fakeAgent();
  render(<Settings />);
  const select = await screen.findByRole('combobox', { name: 'Missed media policy' }, slow) as HTMLSelectElement;
  await waitFor(() => expect(select.value).toBe('ASK'), slow);
  fireEvent.change(select, { target: { value: 'AUTOMATIC' } });
  await waitFor(() => expect(agent.policy).toBe('AUTOMATIC'), slow);
  expect(select.value).toBe('AUTOMATIC');
});
