import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, expect, it, vi } from 'vitest';
import UpdatesPanel from './UpdatesPanel';

const slow = { timeout: 4000 };
const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
afterEach(() => { cleanup(); vi.unstubAllGlobals(); });

function fakeAgent(initial: Partial<Record<string, unknown>> = {}) {
  const state = {
    automatic: true, checks: 0, calls: [] as string[],
    base: { current_version: '0.0.1-dev', channel: 'development', available: null, automatic_install_supported: false,
      discovery: { state: 'NOT_CHECKED', checked_at: null }, ...initial } as Record<string, unknown>,
  };
  const view = () => ({ ...state.base, automatic_updates: state.automatic });
  vi.stubGlobal('fetch', vi.fn(async (url: string, options: RequestInit = {}) => {
    const method = options.method ?? 'GET';
    state.calls.push(`${method} ${url}`);
    if (url === '/api/v1/updates') return json(view());
    if (url === '/api/v1/updates/check' && method === 'POST' && options.body === '{}') {
      state.checks++;
      state.base = { ...state.base, available: { version: '0.0.2-dev', channel: 'development', published_at: '2026-10-26T00:00:00Z' },
        discovery: { state: 'UPDATE_AVAILABLE', checked_at: '2026-10-25T20:00:00Z' } };
      return json(view());
    }
    if (url === '/api/v1/settings/automatic-updates' && method === 'PUT') {
      state.automatic = JSON.parse(String(options.body)).enabled;
      return json({ enabled: state.automatic });
    }
    return json({ error: 'not_found' }, 404);
  }));
  return state;
}

const row = (label: string) => screen.getByText(label, { selector: 'dt' }).closest('.status-row')?.querySelector('dd')?.textContent;

it('shows current version, available version, channel and automatic update status', async () => {
  fakeAgent();
  render(<UpdatesPanel />);
  await screen.findByText('Current version', undefined, slow);
  expect(row('Current version')).toBe('0.0.1-dev');
  expect(row('Available version')).toBe('None');
  expect(row('Update channel')).toBe('Development');
  expect(row('Automatic updates')).toBe('On');
  expect(row('Last checked')).toBe('Never');
  expect(screen.getByText(/discovers updates only; it does not install them yet/)).toBeTruthy();
});

it('detects a newer release when the operator checks', async () => {
  const agent = fakeAgent();
  render(<UpdatesPanel />);
  fireEvent.click(await screen.findByRole('button', { name: 'Check for updates' }, slow));
  await waitFor(() => expect(row('Available version')).toBe('0.0.2-dev'), slow);
  expect(screen.getByText('ArkNet 0.0.2-dev is available on the development channel.')).toBeTruthy();
  expect(row('Last checked')).toContain('Oct 25, 2026');
  expect(agent.checks).toBe(1);
});

it('lets the operator turn automatic updates off', async () => {
  const agent = fakeAgent();
  render(<UpdatesPanel />);
  const toggle = await screen.findByRole('checkbox', { name: 'Automatic updates' }, slow) as HTMLInputElement;
  expect(toggle.checked).toBe(true);
  fireEvent.click(toggle);
  await waitFor(() => expect(row('Automatic updates')).toBe('Off'), slow);
  expect(agent.automatic).toBe(false);
  expect(toggle.checked).toBe(false);
  expect(screen.getByText(/will not install releases automatically/)).toBeTruthy();
});

it.each([
  ['NOT_CONFIGURED', /No ArkNet release index is configured/],
  ['NO_CURRENT_RELEASE', /running release is unknown/],
])('disables checking with an explanation when %s', async (state, message) => {
  fakeAgent({ discovery: { state, checked_at: null }, ...(state === 'NO_CURRENT_RELEASE' ? { current_version: null, channel: null } : {}) });
  render(<UpdatesPanel />);
  expect(await screen.findByText(message, undefined, slow)).toBeTruthy();
  expect((screen.getByRole('button', { name: 'Check for updates' }) as HTMLButtonElement).disabled).toBe(true);
});

it('explains a failed check without hiding an earlier verified release', async () => {
  fakeAgent({ discovery: { state: 'CHECK_FAILED', checked_at: '2026-10-25T20:00:00Z' }, available: { version: '0.0.2-dev', channel: 'development', published_at: '2026-10-26T00:00:00Z' } });
  render(<UpdatesPanel />);
  expect(await screen.findByText('The latest check failed; ArkNet 0.0.2-dev was found earlier.', undefined, slow)).toBeTruthy();
});
