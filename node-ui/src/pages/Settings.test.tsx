import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, expect, it, vi } from 'vitest';
import Settings from './Settings';
vi.mock('../api/updates', async importOriginal => ({
  ...await importOriginal<typeof import('../api/updates')>(),
  fetchUpdates: vi.fn().mockResolvedValue({ current_version: '0.0.1-dev', channel: 'development', available: null, automatic_updates: true, automatic_install_supported: false, discovery: { state: 'NOT_CHECKED', checked_at: null } }),
}));
vi.mock('../api/mediaMisses', async importOriginal => ({
  ...await importOriginal<typeof import('../api/mediaMisses')>(),
  fetchMediaMisses: vi.fn().mockResolvedValue({ policy: 'ASK', notifications: [] }),
  fetchMediaMissPolicy: vi.fn().mockResolvedValue('ASK'),
}));

afterEach(() => { cleanup(); vi.unstubAllGlobals(); });

it('shows peer serving off by default and lets the operator explicitly enable it', async () => {
  const fetchMock = vi.fn()
    .mockResolvedValueOnce(new Response('{"enabled":false}', { status: 200 }))
    .mockResolvedValueOnce(new Response('{"enabled":true}', { status: 200 }));
  vi.stubGlobal('fetch', fetchMock);
  render(<Settings />);
  const toggle = await screen.findByRole('checkbox', { name: 'Peer Serving' }) as HTMLInputElement;
  expect(toggle.checked).toBe(false);
  fireEvent.click(toggle);
  await waitFor(() => expect(toggle.checked).toBe(true));
  expect(fetchMock).toHaveBeenLastCalledWith('/api/v1/settings/peer-serving', expect.objectContaining({ method: 'PUT', body: '{"enabled":true}' }));
  expect(screen.getByText(/Receiving content remains available/)).toBeTruthy();
});

it('restores the confirmed value when an update fails', async () => {
  const fetchMock = vi.fn()
    .mockResolvedValueOnce(new Response('{"enabled":true}', { status: 200 }))
    .mockResolvedValueOnce(new Response('unavailable', { status: 503 }));
  vi.stubGlobal('fetch', fetchMock);
  render(<Settings />);
  const toggle = await screen.findByRole('checkbox', { name: 'Peer Serving' }) as HTMLInputElement;
  expect(toggle.checked).toBe(true);
  fireEvent.click(toggle);
  expect(await screen.findByRole('alert')).toBeTruthy();
  expect(toggle.checked).toBe(true);
});
