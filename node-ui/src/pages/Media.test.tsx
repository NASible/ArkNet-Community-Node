import { act, cleanup, fireEvent, render, screen, within } from '@testing-library/react';
import { afterEach, expect, it, vi } from 'vitest';
import Media from './Media';
vi.mock('../api/mediaMisses', async importOriginal => ({
  ...await importOriginal<typeof import('../api/mediaMisses')>(),
  fetchMediaMisses: vi.fn().mockResolvedValue({ policy: 'ASK', notifications: [] }),
  fetchMediaMissPolicy: vi.fn().mockResolvedValue('ASK'),
}));
// Keep queue request sequencing isolated; real transfer integration is covered in Media.transfer.test.tsx.
vi.mock('../api/transfer', async importOriginal => ({
  ...await importOriginal<typeof import('../api/transfer')>(),
  fetchTransfers: vi.fn().mockResolvedValue({ enabled: false, available: false, transfers: [] }),
}));

const object = { id: `media_${'a'.repeat(64)}`, title: 'A film', size: 1024, state: 'ACTIVE', local_state: 'NOT_PRESENT' };
const catalog = { objects: [object], allocation_bytes: 4096, reserved_bytes: 0, core_available: true };
const response = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
afterEach(() => { cleanup(); vi.unstubAllGlobals(); vi.useRealTimers(); });

it('loads media with title, readable size, local state, allocation and queue-only copy', async () => {
  vi.stubGlobal('fetch', vi.fn().mockResolvedValue(response(catalog)));
  render(<Media />);
  expect(screen.getByRole('status').textContent).toMatch(/Loading media/);
  expect(await screen.findByText('A film')).toBeTruthy();
  expect(screen.getByText('1 KiB')).toBeTruthy();
  expect(screen.getByText('Not present')).toBeTruthy();
  expect(screen.getByText('4 KiB')).toBeTruthy();
  expect(screen.getByText('0 B')).toBeTruthy();
  expect(screen.getByText(/queue only.*transfers require.*explicit start/i)).toBeTruthy();
  expect((screen.getByRole('button', { name: 'Make Available Locally' }) as HTMLButtonElement).disabled).toBe(false);
});

it('lets the operator delete only a removed retained copy', async () => {
 const retained = { ...object, state: 'REMOVED', local_state: 'REMOVED_RETAINED' };
 const fetchMock = vi.fn().mockResolvedValueOnce(response({ ...catalog, objects: [retained], reserved_bytes: 1024 }))
  .mockResolvedValueOnce(new Response(null, { status: 204 }))
  .mockResolvedValueOnce(response({ ...catalog, objects: [{ ...retained, local_state: 'NOT_PRESENT' }], reserved_bytes: 0 }));
 vi.stubGlobal('fetch', fetchMock);
 render(<Media />);
 fireEvent.click(await screen.findByRole('button', { name: 'Delete retained local copy' }));
 expect(await screen.findByText('Not present')).toBeTruthy();
 expect(screen.getByText('0 B')).toBeTruthy();
 expect(screen.queryByRole('button', { name: 'Delete retained local copy' })).toBeNull();
 expect(fetchMock.mock.calls.map(([, options]) => options?.method ?? 'GET')).toEqual(['GET', 'DELETE', 'GET']);
});

it('displays unlimited allocation and explains GiB configuration without formatting the sentinel as bytes', async () => {
  vi.stubGlobal('fetch', vi.fn().mockResolvedValue(response({ ...catalog, allocation_bytes: -1 })));
  render(<Media />);
  expect(await screen.findByText('Unlimited')).toBeTruthy();
  expect(screen.queryByText('-1 B')).toBeNull();
  expect(screen.getByText(/positive allocation in GiB or -1 for unlimited/i)).toBeTruthy();
});

it.each([
 ['NOT_PRESENT', 'Not present', true], ['QUEUED', 'Queued', false], ['DOWNLOADING', 'Downloading', false],
 ['AVAILABLE', 'Available', false], ['REMOVED_RETAINED', 'Removed — retained locally', false], ['ERROR', 'Error', false],
])('renders local state %s without offering duplicate selection', async (local_state, label, selectable) => {
 vi.stubGlobal('fetch', vi.fn().mockResolvedValue(response({ ...catalog, objects: [{ ...object, local_state }] })));
 render(<Media />);
 expect(await screen.findByText(label)).toBeTruthy();
 expect(Boolean(screen.queryByRole('button', { name: 'Make Available Locally' }))).toBe(selectable);
});
it('shows removed catalog entries without selection', async () => {
 vi.stubGlobal('fetch', vi.fn().mockResolvedValue(response({ ...catalog, objects: [{ ...object, state: 'REMOVED' }] })));
 render(<Media />);
 expect(await screen.findByText('Removed from catalog')).toBeTruthy();
 expect(screen.queryByRole('button', { name: 'Make Available Locally' })).toBeNull();
});
it.each([
 { ...catalog, core_available: false }, { ...catalog, allocation_bytes: 0 },
 { ...catalog, allocation_bytes: -1, core_available: false },
 { ...catalog, reserved_bytes: 4096 }, { ...catalog, allocation_bytes: 100 },
 { ...catalog, allocation_bytes: 0, objects: [{ ...object, size: 0 }] },
])('disables new selection when offline or outside allocation %#', async body => {
 vi.stubGlobal('fetch', vi.fn().mockResolvedValue(response(body)));
 render(<Media />);
 const button = await screen.findByRole('button', { name: 'Make Available Locally' });
 expect((button as HTMLButtonElement).disabled).toBe(true);
 if (!body.core_available) expect(screen.getByText(/Core is offline.*saved selections/i)).toBeTruthy();
});
it('shows an explicit empty catalog', async () => {
 vi.stubGlobal('fetch', vi.fn().mockResolvedValue(response({ ...catalog, objects: [] })));
 render(<Media />);
 expect(await screen.findByText('No media is available in this catalog.')).toBeTruthy();
});
it('retries a failed catalog GET without a mutation', async () => {
 const fetchMock = vi.fn().mockRejectedValueOnce(new TypeError('network')).mockResolvedValueOnce(response(catalog));
 vi.stubGlobal('fetch', fetchMock);
 render(<Media />);
 expect((await screen.findByRole('alert')).textContent).toMatch(/Cannot reach.*Agent/i);
 fireEvent.click(screen.getByRole('button', { name: 'Retry media refresh' }));
 expect(await screen.findByText('A film')).toBeTruthy();
 expect(fetchMock).toHaveBeenCalledTimes(2);
});
it('times out requests after ten seconds', async () => {
 vi.useFakeTimers();
 vi.stubGlobal('fetch', vi.fn((_url, options: RequestInit) => new Promise((_resolve, reject) => {
   options.signal?.addEventListener('abort', () => reject(new DOMException('Aborted', 'AbortError')));
 })));
 render(<Media />);
 await act(() => vi.advanceTimersByTimeAsync(10000));
 expect(screen.getByRole('alert').textContent).toMatch(/timed out/i);
});
it('aborts outstanding requests and clears timers on unmount', () => {
 vi.useFakeTimers();
 const fetchMock = vi.fn(() => new Promise(() => {}));
 vi.stubGlobal('fetch', fetchMock);
 const { unmount } = render(<Media />);
 const options = fetchMock.mock.calls[0] as unknown as [string, RequestInit];
 unmount();
 expect(options[1].signal?.aborted).toBe(true);
 expect(vi.getTimerCount()).toBe(0);
});

it('queues unlimited media only on operator action and keeps authoritative refresh and pending locks', async () => {
 let finishPost!: (value: Response) => void;
 let finishGet!: (value: Response) => void;
 const large = { ...object, size: Number.MAX_SAFE_INTEGER };
 const second = { ...object, id: `media_${'b'.repeat(64)}`, title: 'Second film' };
 const unlimited = { ...catalog, allocation_bytes: -1, reserved_bytes: 8192, objects: [large, second] };
 const fetchMock = vi.fn().mockResolvedValueOnce(response(unlimited))
  .mockImplementationOnce(() => new Promise<Response>(resolve => { finishPost = resolve; }))
  .mockImplementationOnce(() => new Promise<Response>(resolve => { finishGet = resolve; }));
 vi.stubGlobal('fetch', fetchMock);
 render(<Media />);
 await screen.findByText('A film');
 expect(fetchMock).toHaveBeenCalledTimes(1);
 expect(screen.getByText(/queue only.*transfers require.*explicit start/i)).toBeTruthy();
 const buttons = screen.getAllByRole('button', { name: 'Make Available Locally' });
 buttons.forEach(button => expect((button as HTMLButtonElement).disabled).toBe(false));
 fireEvent.click(buttons[0]); fireEvent.click(buttons[0]);
 expect(fetchMock).toHaveBeenCalledTimes(2);
 buttons.forEach(button => expect((button as HTMLButtonElement).disabled).toBe(true));
 await act(async () => finishPost(response({ ...large, local_state: 'QUEUED' })));
 expect(fetchMock).toHaveBeenCalledTimes(3);
 expect((screen.getByRole('button', { name: 'Make Available Locally' }) as HTMLButtonElement).disabled).toBe(true);
 await act(async () => finishGet(response({ ...catalog, allocation_bytes: 2048, reserved_bytes: 1536,
  objects: [{ ...large, title: 'Renamed film', local_state: 'QUEUED' }, second] })));
 expect(screen.getByText('Renamed film')).toBeTruthy();
 expect(screen.getByText('Queued')).toBeTruthy();
 expect(screen.getByText('2 KiB')).toBeTruthy();
 expect(screen.getByText('1.5 KiB')).toBeTruthy();
 expect(screen.queryByText('Unlimited')).toBeNull();
 expect((screen.getByRole('button', { name: 'Make Available Locally' }) as HTMLButtonElement).disabled).toBe(true);
 expect(fetchMock.mock.calls.map(([, options]) => options.method ?? 'GET')).toEqual(['GET', 'POST', 'GET']);
 fetchMock.mock.calls.forEach(([, options]) => expect(options.cache).toBe('no-store'));
});

it('queues once, locks all controls during POST/refetch and uses authoritative reserved bytes', async () => {
 let finishPost!: (value: Response) => void;
 let finishGet!: (value: Response) => void;
 const second = { ...object, id: `media_${'b'.repeat(64)}`, title: 'Second film' };
 const fetchMock = vi.fn().mockResolvedValueOnce(response({ ...catalog, objects: [object, second] }))
   .mockImplementationOnce(() => new Promise<Response>(resolve => { finishPost = resolve; }))
   .mockImplementationOnce(() => new Promise<Response>(resolve => { finishGet = resolve; }));
 vi.stubGlobal('fetch', fetchMock);
 render(<Media />);
 await screen.findByText('A film');
 const buttons = screen.getAllByRole('button', { name: 'Make Available Locally' });
 fireEvent.click(buttons[0]); fireEvent.click(buttons[0]);
 expect(fetchMock).toHaveBeenCalledTimes(2);
 buttons.forEach(button => expect((button as HTMLButtonElement).disabled).toBe(true));
 await act(async () => finishPost(response({ ...object, local_state: 'QUEUED' })));
 expect(fetchMock).toHaveBeenCalledTimes(3);
 expect((screen.getByRole('button', { name: 'Make Available Locally' }) as HTMLButtonElement).disabled).toBe(true);
 await act(async () => finishGet(response({ ...catalog, objects: [{ ...object, local_state: 'QUEUED' }, second], reserved_bytes: 3072 })));
 expect(screen.getByText('Queued')).toBeTruthy();
 expect(screen.getByText('3 KiB')).toBeTruthy();
 expect(screen.getAllByRole('button', { name: 'Make Available Locally' })).toHaveLength(1);
});
it.each([
 [4096, 'mutation'], [4096, 'refresh'], [-1, 'mutation'], [-1, 'refresh'],
])('keeps allocation %s selection locked after %s failure until an explicit successful GET retry', async (allocation_bytes, phase) => {
 const fetchMock = vi.fn().mockResolvedValueOnce(response({ ...catalog, allocation_bytes }));
 if (phase === 'mutation') fetchMock.mockRejectedValueOnce(new TypeError('lost response'));
 else fetchMock.mockResolvedValueOnce(response({ ...object, local_state: 'QUEUED' })).mockRejectedValueOnce(new TypeError('refresh failed'));
 fetchMock.mockResolvedValueOnce(response({ ...catalog, objects: [{ ...object, local_state: 'QUEUED' }], reserved_bytes: 1024 }));
 vi.stubGlobal('fetch', fetchMock);
 render(<Media />);
 fireEvent.click(await screen.findByRole('button', { name: 'Make Available Locally' }));
 expect((await screen.findByRole('alert')).textContent).toMatch(/refresh.*before.*another selection/i);
 screen.queryAllByRole('button', { name: 'Make Available Locally' }).forEach(button => expect((button as HTMLButtonElement).disabled).toBe(true));
 fireEvent.click(screen.getByRole('button', { name: 'Retry media refresh' }));
 expect(await screen.findByText('Queued')).toBeTruthy();
 expect(screen.queryByRole('button', { name: 'Make Available Locally' })).toBeNull();
 expect(fetchMock.mock.calls.filter(([, options]) => options?.method === 'POST')).toHaveLength(1);
});
it('shows the backend allocation rejection and requires refresh before further selections', async () => {
 vi.stubGlobal('fetch', vi.fn().mockResolvedValueOnce(response(catalog))
   .mockResolvedValueOnce(response({ error: 'storage_allocation_exceeded' }, 409)));
 render(<Media />);
 fireEvent.click(await screen.findByRole('button', { name: 'Make Available Locally' }));
 expect((await screen.findByRole('alert')).textContent).toMatch(/Storage allocation exceeded/);
 expect((screen.getByRole('button', { name: 'Make Available Locally' }) as HTMLButtonElement).disabled).toBe(true);
});
it('does not re-offer a confirmed queued selection even if refetch returns stale NOT_PRESENT', async () => {
 vi.stubGlobal('fetch', vi.fn().mockResolvedValueOnce(response(catalog))
  .mockResolvedValueOnce(response({ ...object, local_state: 'QUEUED' })).mockResolvedValueOnce(response(catalog)));
 render(<Media />);
 fireEvent.click(await screen.findByRole('button', { name: 'Make Available Locally' }));
 await act(async () => {});
 expect(screen.queryByRole('button', { name: 'Make Available Locally' })).toBeNull();
 expect(screen.getByText('Queued')).toBeTruthy();
});
it('aborts a pending mutation on unmount', async () => {
 const fetchMock = vi.fn().mockResolvedValueOnce(response(catalog)).mockImplementationOnce(() => new Promise(() => {}));
 vi.stubGlobal('fetch', fetchMock);
 const { unmount } = render(<Media />);
 fireEvent.click(await screen.findByRole('button', { name: 'Make Available Locally' }));
 const signal = fetchMock.mock.calls[1][1].signal;
 unmount();
 expect(signal.aborted).toBe(true);
});

it.each(['', '   '])('renders a readable fallback for blank titles %j', async title => {
 vi.stubGlobal('fetch', vi.fn().mockResolvedValue(response({ ...catalog, objects: [{ ...object, title }] })));
 render(<Media />);
 expect(await screen.findByText('Untitled media')).toBeTruthy();
});

it('preserves authoritative removal and renamed title after a confirmed selection', async () => {
 vi.stubGlobal('fetch', vi.fn().mockResolvedValueOnce(response(catalog))
  .mockResolvedValueOnce(response({ ...object, local_state: 'QUEUED' }))
  .mockResolvedValueOnce(response({ ...catalog, objects: [{ ...object, title: 'Renamed film', state: 'REMOVED' }] })));
 render(<Media />);
 fireEvent.click(await screen.findByRole('button', { name: 'Make Available Locally' }));
 expect(await screen.findByText('Removed from catalog')).toBeTruthy();
 expect(screen.getByText('Renamed film')).toBeTruthy();
 expect(screen.queryByRole('button', { name: 'Make Available Locally' })).toBeNull();
});
