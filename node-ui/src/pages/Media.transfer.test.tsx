import { act, cleanup, fireEvent, render, screen, within } from '@testing-library/react';
import { afterEach, expect, it, vi } from 'vitest';
import Media from './Media';
vi.mock('../api/mediaMisses', async importOriginal => ({
  ...await importOriginal<typeof import('../api/mediaMisses')>(),
  fetchMediaMisses: vi.fn().mockResolvedValue({ policy: 'ASK', notifications: [] }),
  fetchMediaMissPolicy: vi.fn().mockResolvedValue('ASK'),
}));
const object = { id: `media_${'a'.repeat(64)}`, title: 'A film', size: 100, state: 'ACTIVE', local_state: 'NOT_PRESENT' };
const catalog = { objects: [object], allocation_bytes: 4096, reserved_bytes: 0, core_available: true };
const status = { object_id: object.id, state: 'COMPLETED', total_bytes: 100, completed_bytes: 100, mock: true, error_code: '' };
const response = (data: unknown, code = 200) => new Response(JSON.stringify(data), { status: code });
afterEach(() => { cleanup(); vi.unstubAllGlobals(); });
it('explains that one action selects and starts the transfer', async () => {
 vi.stubGlobal('fetch', vi.fn((url: string) => url === '/api/v1/transfers'
  ? Promise.resolve(response({ enabled: true, available: true, transfers: [] }))
  : Promise.resolve(response(catalog))));
 render(<Media />);
 expect(await screen.findByText('Make media available with one action. Progress and controls appear below.')).toBeTruthy();
});
it('selects and starts transfer from one operator action without inventing local availability', async () => {
 let queued = false; let started = false;
 const mock = vi.fn((url: string, options: RequestInit) => {
  if (url.endsWith('/selection')) { queued = true; return Promise.resolve(response({ ...object, local_state: 'QUEUED' })); }
  if (url.endsWith('/transfer/start')) { started = true; return Promise.resolve(response(status)); }
  if (url === '/api/v1/transfers') return Promise.resolve(response({ enabled: true, available: true, transfers: started ? [status] : [] }));
  return Promise.resolve(response({ ...catalog, objects: [{ ...object, local_state: queued ? 'QUEUED' : 'NOT_PRESENT' }] }));
 }); vi.stubGlobal('fetch', mock);
 render(<Media />);
 fireEvent.click(await screen.findByRole('button', { name: 'Make Available Locally' }));
 expect(await screen.findByText('Mock complete — no media downloaded')).toBeTruthy();
 expect(started).toBe(true);
 expect(screen.getByText('Queued')).toBeTruthy();
 expect(screen.queryByText('Available')).toBeNull();
 expect(screen.queryByRole('button', { name: 'Make Available Locally' })).toBeNull();
 expect(screen.queryByRole('button', { name: 'Start transfer' })).toBeNull();
 expect(mock.mock.calls.filter(([, options]) => options.method === 'POST')).toHaveLength(2);
});
it('isolates transfer failure from a working catalog and selection', async () => {
 const mock = vi.fn((url: string, options: RequestInit) => {
  if (url === '/api/v1/transfers') return Promise.reject(new TypeError('offline'));
  return Promise.resolve(response(options.method === 'POST' ? { ...object, local_state: 'QUEUED' } : catalog));
 }); vi.stubGlobal('fetch', mock);
 render(<Media />);
 const panel = await screen.findByRole('region', { name: 'Transfers' });
 expect(await within(panel).findByRole('alert')).toBeTruthy();
 fireEvent.click(await screen.findByRole('button', { name: 'Make Available Locally' }));
 expect(await screen.findByText('Queued')).toBeTruthy();
 expect(mock.mock.calls.filter(([, options]) => options.method === 'POST')).toHaveLength(1);
});
it('shows transfer status even when the catalog fails', async () => {
 vi.stubGlobal('fetch', vi.fn((url: string) => url === '/api/v1/transfers'
  ? Promise.resolve(response({ enabled: true, available: true, transfers: [status] })) : Promise.reject(new TypeError('catalog offline'))));
 render(<Media />);
 expect(await screen.findByText('Mock complete — no media downloaded')).toBeTruthy();
 expect(screen.getByText(/Cannot reach Node Agent/)).toBeTruthy();
});
it('shows real completion without promoting queued media to available', async () => {
 const real = { ...status, mock: false };
 vi.stubGlobal('fetch', vi.fn((url: string) => url === '/api/v1/transfers'
  ? Promise.resolve(response({ enabled: true, available: true, transfers: [real] }))
  : Promise.resolve(response({ ...catalog, objects: [{ ...object, local_state: 'QUEUED' }] }))));
 render(<Media />);
 expect(await screen.findByText('Verified transfer complete — awaiting catalog confirmation of local availability.')).toBeTruthy();
 expect(screen.getByText('Queued')).toBeTruthy();
 expect(screen.queryByText('Available')).toBeNull();
 expect(screen.queryByText(/mock/i)).toBeNull();
});
