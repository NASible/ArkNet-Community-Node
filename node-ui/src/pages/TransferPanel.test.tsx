import { act, cleanup, fireEvent, render, screen, within } from '@testing-library/react';
import { afterEach, expect, it, vi } from 'vitest';
import TransferPanel from './TransferPanel';
import type { MediaObject } from '../api/media';
const object: MediaObject = { id: `media_${'a'.repeat(64)}`, title: 'A film', size: 100, state: 'ACTIVE', local_state: 'QUEUED' };
const status = { object_id: object.id, state: 'DOWNLOADING', total_bytes: 100, completed_bytes: 25, mock: true, error_code: '' };
const body = { enabled: true, available: true, transfers: [status] };
const response = (data: unknown, code = 200) => new Response(JSON.stringify(data), { status: code });
afterEach(() => { cleanup(); vi.unstubAllGlobals(); vi.useRealTimers(); });
it('polls sequentially one second after completion, preserves stale progress and recovers', async () => {
 vi.useFakeTimers();
 let finish!: (value: Response) => void;
 const mock = vi.fn().mockResolvedValueOnce(response(body))
  .mockImplementationOnce(() => new Promise<Response>(resolve => { finish = resolve; }))
  .mockRejectedValueOnce(new TypeError('offline'))
  .mockResolvedValue(response({ ...body, transfers: [{ ...status, completed_bytes: 75 }] }));
 vi.stubGlobal('fetch', mock);
 render(<TransferPanel objects={[object]} />);
 await act(async () => {});
 await act(() => vi.advanceTimersByTimeAsync(999)); expect(mock).toHaveBeenCalledTimes(1);
 await act(() => vi.advanceTimersByTimeAsync(1)); expect(mock).toHaveBeenCalledTimes(2);
 await act(() => vi.advanceTimersByTimeAsync(3000)); expect(mock).toHaveBeenCalledTimes(2);
 await act(async () => finish(response({ ...body, transfers: [{ ...status, completed_bytes: 50 }] })));
 expect(screen.getByText('50 / 100 bytes (50%)')).toBeTruthy();
 await act(() => vi.advanceTimersByTimeAsync(1000));
 expect(screen.getByText('50 / 100 bytes (50%)')).toBeTruthy();
 expect(screen.getByRole('alert').textContent).toMatch(/stale.*cannot reach/i);
 await act(() => vi.advanceTimersByTimeAsync(1000));
 expect(screen.getByText('75 / 100 bytes (75%)')).toBeTruthy();
 expect(screen.queryByRole('alert')).toBeNull();
});
it('times out and aborts a pending GET, then clears every timer on unmount', async () => {
 vi.useFakeTimers();
 const mock = vi.fn((_url: string, options: RequestInit) => new Promise<Response>((_resolve, reject) => {
  options.signal?.addEventListener('abort', () => reject(new DOMException('Aborted', 'AbortError')));
 })); vi.stubGlobal('fetch', mock);
 const { unmount } = render(<TransferPanel objects={[object]} />);
 await act(() => vi.advanceTimersByTimeAsync(10000));
 expect(mock.mock.calls[0][1].signal?.aborted).toBe(true);
 expect(screen.getByRole('alert').textContent).toMatch(/timed out/i);
 await act(() => vi.advanceTimersByTimeAsync(1000));
 expect(mock).toHaveBeenCalledTimes(2);
 unmount();
 expect(mock.mock.calls[1][1].signal?.aborted).toBe(true);
 expect(vi.getTimerCount()).toBe(0);
});

it('runs only explicit start, pause, resume and cancel with authoritative refetch and one global lock', async () => {
 let state = '';
 let finish!: (response: Response) => void;
 const mock = vi.fn((url: string, options: RequestInit) => {
  if (options.method === 'POST') {
   const action = url.split('/').pop();
   state = action === 'pause' ? 'PAUSED' : action === 'cancel' ? 'CANCELLED' : 'DOWNLOADING';
   if (action === 'start') return new Promise<Response>(resolve => { finish = resolve; });
   return Promise.resolve(response({ ...status, state }));
  }
  return Promise.resolve(response({ ...body, transfers: state ? [{ ...status, state }] : [] }));
 }); vi.stubGlobal('fetch', mock);
 const second = { ...object, id: `media_${'b'.repeat(64)}`, title: 'Second' };
 render(<TransferPanel objects={[object, second]} />);
 const starts = await screen.findAllByRole('button', { name: 'Start transfer' });
 expect(mock).toHaveBeenCalledTimes(1);
 fireEvent.click(starts[0]); fireEvent.click(starts[1]);
 expect(mock).toHaveBeenCalledTimes(2);
 starts.forEach(button => expect((button as HTMLButtonElement).disabled).toBe(true));
 await act(async () => finish(response(status)));
 expect(mock).toHaveBeenCalledTimes(3);
 fireEvent.click(screen.getByRole('button', { name: 'Pause mock transfer' }));
 expect(await screen.findByText('PAUSED')).toBeTruthy();
 fireEvent.click(screen.getByRole('button', { name: 'Resume mock transfer' }));
 expect(await screen.findByText('DOWNLOADING')).toBeTruthy();
 fireEvent.click(screen.getByRole('button', { name: 'Cancel mock transfer' }));
 expect(await screen.findByText('CANCELLED')).toBeTruthy();
 expect(mock.mock.calls.filter(([, options]) => options.method === 'POST').map(([url]) => url.split('/').pop())).toEqual(['start','pause','resume','cancel']);
 expect(screen.queryByRole('button', { name: 'Pause mock transfer' })).toBeNull();
});
it.each([
 ['NOT_PRESENT', 'ACTIVE'], ['AVAILABLE', 'ACTIVE'], ['DOWNLOADING', 'ACTIVE'], ['QUEUED', 'REMOVED'],
] as const)('does not start unselected or ineligible media %s %s', async (local_state, state) => {
 vi.stubGlobal('fetch', vi.fn().mockResolvedValue(response({ ...body, transfers: [] })));
 render(<TransferPanel objects={[{ ...object, local_state, state }]} />);
 await act(async () => {});
 expect(screen.queryByRole('button', { name: 'Start transfer' })).toBeNull();
});
it.each(['QUEUED', 'ERROR'] as const)('offers separate explicit start for %s only when no transfer exists', async local_state => {
 vi.stubGlobal('fetch', vi.fn().mockResolvedValue(response({ ...body, transfers: [] })));
 render(<TransferPanel objects={[{ ...object, local_state }]} />);
 expect((await screen.findByRole('button', { name: 'Start transfer' }) as HTMLButtonElement).disabled).toBe(false);
});

it.each([false, true])('explains disabled/unavailable service (enabled=%s) and prevents every action', async enabled => {
 vi.stubGlobal('fetch', vi.fn().mockResolvedValue(response({ ...body, enabled, available: false })));
 render(<TransferPanel objects={[object, { ...object, id: `media_${'b'.repeat(64)}` }]} />);
 expect(await screen.findByText(enabled ? /Transfer service unavailable/ : /Transfer service not configured/)).toBeTruthy();
 if (!enabled) expect(screen.queryAllByRole('button')).toHaveLength(0);
 else screen.getAllByRole('button').forEach(button => expect((button as HTMLButtonElement).disabled).toBe(true));
});
it.each([
 ['COMPLETED', '', 'Mock complete — no media downloaded', 100],
 ['VERIFICATION_FAILED', 'verification_failed', 'Mock verification failed — no media downloaded or verified', 0],
 ['CANCELLED', '', 'Mock cancelled — no media downloaded', 0],
])('makes zero-byte terminal state %s explicitly synthetic', async (state, error_code, label, percent) => {
 vi.stubGlobal('fetch', vi.fn().mockResolvedValue(response({ ...body, transfers: [{ ...status, state, error_code, total_bytes: 0, completed_bytes: 0 }] })));
 render(<TransferPanel objects={[object]} />);
 expect(await screen.findByText(label as string)).toBeTruthy();
 expect(screen.getByText(`0 / 0 bytes (${percent}%)`)).toBeTruthy();
 expect(screen.queryByRole('button', { name: /Start|Resume|Pause|Cancel mock transfer/ })).toBeNull();
 expect(screen.queryByText('Available')).toBeNull();
});
it.each(['mutation', 'refetch'])('locks after %s failure until an explicit successful refresh', async phase => {
 const mock = vi.fn().mockResolvedValueOnce(response(body));
 if (phase === 'mutation') mock.mockRejectedValueOnce(new TypeError('lost response'));
 else mock.mockResolvedValueOnce(response({ ...status, state: 'PAUSED' })).mockRejectedValueOnce(new TypeError('offline'));
 mock.mockResolvedValue(response({ ...body, transfers: [{ ...status, state: 'PAUSED' }] }));
 vi.stubGlobal('fetch', mock);
 render(<TransferPanel objects={[object]} />);
 fireEvent.click(await screen.findByRole('button', { name: 'Pause mock transfer' }));
 expect((await screen.findByRole('alert')).textContent).toMatch(/Refresh transfers before.*controls/i);
 expect(screen.getByText('25 / 100 bytes (25%)')).toBeTruthy();
 expect((screen.getByRole('button', { name: 'Pause mock transfer' }) as HTMLButtonElement).disabled).toBe(true);
 fireEvent.click(screen.getByRole('button', { name: 'Refresh transfers' }));
 expect(await screen.findByText('PAUSED')).toBeTruthy();
 expect((screen.getByRole('button', { name: 'Resume mock transfer' }) as HTMLButtonElement).disabled).toBe(false);
 expect(mock.mock.calls.filter(([, options]) => options.method === 'POST')).toHaveLength(1);
});
it('retains the global lock until authoritative GET completes and aborts it on unmount', async () => {
 vi.useFakeTimers();
 const mock = vi.fn().mockResolvedValueOnce(response(body)).mockResolvedValueOnce(response({ ...status, state: 'PAUSED' }))
  .mockImplementationOnce(() => new Promise(() => {}));
 vi.stubGlobal('fetch', mock);
 const { unmount } = render(<TransferPanel objects={[object]} />);
 await act(async () => {});
 fireEvent.click(screen.getByRole('button', { name: 'Pause mock transfer' }));
 await act(async () => {});
 expect(mock).toHaveBeenCalledTimes(3);
 expect((screen.getByRole('button', { name: 'Pause mock transfer' }) as HTMLButtonElement).disabled).toBe(true);
 await act(() => vi.advanceTimersByTimeAsync(2000));
 expect(mock).toHaveBeenCalledTimes(3);
 unmount();
 expect(mock.mock.calls[2][1].signal.aborted).toBe(true);
 expect(vi.getTimerCount()).toBe(0);
});

it('revalidates service availability after an in-flight poll before posting a control', async () => {
 vi.useFakeTimers();
 let finish!: (value: Response) => void;
 const mock = vi.fn().mockResolvedValueOnce(response(body))
  .mockImplementationOnce(() => new Promise<Response>(resolve => { finish = resolve; }));
 vi.stubGlobal('fetch', mock);
 render(<TransferPanel objects={[object]} />);
 await act(async () => {});
 await act(() => vi.advanceTimersByTimeAsync(1000));
 fireEvent.click(screen.getByRole('button', { name: 'Pause mock transfer' }));
 await act(async () => finish(response({ ...body, available: false })));
 expect(mock).toHaveBeenCalledTimes(2);
 expect(mock.mock.calls.some(([, options]) => options.method === 'POST')).toBe(false);
});
it('revalidates changed terminal status after a pending poll before posting', async () => {
 vi.useFakeTimers();
 let finish!: (value: Response) => void;
 const mock = vi.fn().mockResolvedValueOnce(response(body))
  .mockImplementationOnce(() => new Promise<Response>(resolve => { finish = resolve; }));
 vi.stubGlobal('fetch', mock);
 render(<TransferPanel objects={[object]} />);
 await act(async () => {});
 await act(() => vi.advanceTimersByTimeAsync(1000));
 fireEvent.click(screen.getByRole('button', { name: 'Pause mock transfer' }));
 await act(async () => finish(response({ ...body, transfers: [{ ...status, state: 'COMPLETED', completed_bytes: 100 }] })));
 expect(mock).toHaveBeenCalledTimes(2);
});
it('does not offer transfer start until the media queue safeguards have cleared', async () => {
 vi.stubGlobal('fetch', vi.fn().mockResolvedValue(response({ ...body, transfers: [] })));
 const { rerender } = render(<TransferPanel objects={[object]} startAllowed={false} />);
 expect((await screen.findByRole('button', { name: 'Start transfer' }) as HTMLButtonElement).disabled).toBe(true);
 rerender(<TransferPanel objects={[object]} startAllowed />);
 expect((screen.getByRole('button', { name: 'Start transfer' }) as HTMLButtonElement).disabled).toBe(false);
});
it('aborts a pending POST and clears timers on unmount without an authoritative fetch', async () => {
 vi.useFakeTimers();
 const mock = vi.fn().mockResolvedValueOnce(response(body)).mockImplementationOnce(() => new Promise(() => {}));
 vi.stubGlobal('fetch', mock);
 const { unmount } = render(<TransferPanel objects={[object]} />);
 await act(async () => {});
 fireEvent.click(screen.getByRole('button', { name: 'Pause mock transfer' }));
 unmount();
 expect(mock.mock.calls[1][1].signal.aborted).toBe(true);
 expect(vi.getTimerCount()).toBe(0);
 expect(mock).toHaveBeenCalledTimes(2);
});

it('labels real progress and controls without synthetic wording', async () => {
 const real = { ...status, mock: false };
 vi.stubGlobal('fetch', vi.fn().mockResolvedValue(response({ ...body, transfers: [real] })));
 render(<TransferPanel objects={[object]} />);
 const panel = await screen.findByRole('region', { name: 'Transfers' });
 expect(await within(panel).findByText('Transfer — A film')).toBeTruthy();
 expect(within(panel).getByRole('progressbar', { name: 'Transfer progress for A film' })).toBeTruthy();
 expect(within(panel).getByRole('button', { name: 'Pause transfer' })).toBeTruthy();
 expect(within(panel).getByRole('button', { name: 'Cancel transfer' })).toBeTruthy();
 expect(panel.textContent).not.toMatch(/mock|synthetic/i);
});

it('reports real completion conservatively until the media catalog says available', async () => {
 const real = { ...status, state: 'COMPLETED', completed_bytes: 100, mock: false };
 vi.stubGlobal('fetch', vi.fn().mockResolvedValue(response({ ...body, transfers: [real] })));
 render(<TransferPanel objects={[object]} />);
 expect(await screen.findByText('Verified transfer complete — awaiting catalog confirmation of local availability.')).toBeTruthy();
 expect(screen.queryByText('Available')).toBeNull();
 expect(screen.queryByText(/mock/i)).toBeNull();
});

it('reports catalog-backed availability only when the catalog says available', async () => {
 const real = { ...status, state: 'COMPLETED', completed_bytes: 100, mock: false };
 vi.stubGlobal('fetch', vi.fn().mockResolvedValue(response({ ...body, transfers: [real] })));
 render(<TransferPanel objects={[{ ...object, local_state: 'AVAILABLE' }]} />);
 expect(await screen.findByText('Verified transfer complete — media catalog reports available locally.')).toBeTruthy();
});

it('labels per-object synthetic progress without starting anything automatically', async () => {
 const mock = vi.fn().mockResolvedValue(response(body)); vi.stubGlobal('fetch', mock);
 render(<TransferPanel objects={[object]} />);
 expect(await screen.findByText('25 / 100 bytes (25%)')).toBeTruthy();
 expect(screen.getByText(/Synthetic progress.*no media.*downloaded/i)).toBeTruthy();
 expect(screen.getByText('DOWNLOADING')).toBeTruthy();
 expect(screen.getByRole('progressbar', { name: 'Mock transfer progress for A film' }).getAttribute('value')).toBe('25');
 expect(mock).toHaveBeenCalledTimes(1);
 expect(mock.mock.calls[0][0]).toBe('/api/v1/transfers');
});
