import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, expect, it, vi } from 'vitest';
import App from './Console';
afterEach(() => { cleanup(); vi.unstubAllGlobals(); });
it('starts on Dashboard and switches to Media and back without automatic media selection', async () => {
 const fetchMock = vi.fn().mockImplementation((url: string) => url === '/api/v1/media'
   ? Promise.resolve(new Response(JSON.stringify({ objects: [], allocation_bytes: 0, reserved_bytes: 0, core_available: false })))
   : new Promise(() => {}));
 vi.stubGlobal('fetch', fetchMock);
 render(<App />);
 expect(screen.getByRole('heading', { name: 'Node dashboard' })).toBeTruthy();
 expect(fetchMock).toHaveBeenCalledTimes(1);
 fireEvent.click(screen.getByRole('button', { name: 'Media' }));
 expect(await screen.findByText('No media is available in this catalog.')).toBeTruthy();
 expect(screen.getByRole('button', { name: 'Media' }).getAttribute('aria-current')).toBe('page');
 fireEvent.click(screen.getByRole('button', { name: 'Dashboard' }));
 expect(screen.getByRole('heading', { name: 'Node dashboard' })).toBeTruthy();
 expect(fetchMock.mock.calls.every(([, options]) => !options?.method)).toBe(true);
});

it('opens the Users management page with local accounts only', async () => {
 const localID = 'local_' + 'a'.repeat(64);
 const fetchMock = vi.fn(async (url: string) => {
   if (url === '/api/v1/local-users') return new Response(JSON.stringify({ users: [{ user_id: localID, username: 'alice', kind: 'LOCAL', status: 'ACTIVE', created_at: '2026-10-25T00:00:00Z' }], truncated: false }));
   return new Response(JSON.stringify({ healthy: true, database_healthy: true }));
 });
 vi.stubGlobal('fetch', fetchMock);
 render(<App />);
 fireEvent.click(screen.getByRole('button', { name: 'Users' }));
 expect(await screen.findByRole('heading', { name: 'Users' })).toBeTruthy();
 expect(screen.getByRole('list', { name: 'Local users' }).textContent).toContain('Localalice');
 expect(screen.queryByRole('list', { name: 'Global members' })).toBeNull();
 expect(fetchMock.mock.calls.some(([url]) => url === '/api/v1/global-members')).toBe(false);
 expect(screen.getByRole('button', { name: 'Users' }).getAttribute('aria-current')).toBe('page');
});
