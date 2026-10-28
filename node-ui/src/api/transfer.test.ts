import { afterEach, expect, it, vi } from 'vitest';
import { fetchTransfers, mutateTransfer } from './transfer';
const status = { object_id: `media_${'a'.repeat(64)}`, state: 'DOWNLOADING', total_bytes: 100, completed_bytes: 25, mock: true, error_code: '' };
const body = { enabled: true, available: true, transfers: [status] };
const response = (data: unknown, code = 200) => new Response(JSON.stringify(data), { status: code });
afterEach(() => vi.unstubAllGlobals());


it.each([
 null, {}, { ...body, enabled: 'true' }, { ...body, available: 1 }, { ...body, extra: true },
 { ...body, transfers: [status, status] },
 ...[
  { object_id: 'media_short' }, { object_id: `media_${'A'.repeat(64)}` }, { state: 'UNKNOWN' },
  { mock: 'true' }, { total_bytes: -1 }, { completed_bytes: -1 },
  { total_bytes: Number.MAX_SAFE_INTEGER + 1 }, { completed_bytes: 1.5 }, { completed_bytes: 101 },
  { state: 'COMPLETED' }, { error_code: 'verification_failed' }, { state: 'VERIFICATION_FAILED' },
  { unexpected: true }, { total_bytes: '100' },
 ].map(patch => ({ ...body, transfers: [{ ...status, ...patch }] })),
])('rejects malformed, duplicate or unknown transfer data %#', async data => {
 vi.stubGlobal('fetch', vi.fn().mockResolvedValue(response(data)));
 await expect(fetchTransfers(new AbortController().signal)).rejects.toMatchObject({ kind: 'invalid-payload' });
});
it('rejects malformed JSON', async () => {
 vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response('bad json')));
 await expect(fetchTransfers(new AbortController().signal)).rejects.toMatchObject({ kind: 'invalid-payload' });
});
it.each(['DOWNLOADING', 'PAUSED', 'CANCELLED', 'COMPLETED', 'VERIFICATION_FAILED'])('accepts zero-byte %s', async state => {
 const data = { ...body, transfers: [{ ...status, state, total_bytes: 0, completed_bytes: 0, error_code: state === 'VERIFICATION_FAILED' ? 'verification_failed' : '' }] };
 vi.stubGlobal('fetch', vi.fn().mockResolvedValue(response(data)));
 await expect(fetchTransfers(new AbortController().signal)).resolves.toEqual(data);
});
it('accepts real transfer progress from the compatible Agent endpoint', async () => {
 const data = { ...body, transfers: [{ ...status, mock: false }] };
 vi.stubGlobal('fetch', vi.fn().mockResolvedValue(response(data)));
 await expect(fetchTransfers(new AbortController().signal)).resolves.toEqual(data);
});
it('fetches validated synthetic progress without caching or credentials', async () => {
 const fetchMock = vi.fn().mockResolvedValue(response(body)); vi.stubGlobal('fetch', fetchMock);
 const signal = new AbortController().signal;
 await expect(fetchTransfers(signal)).resolves.toEqual(body);
 expect(fetchMock).toHaveBeenCalledWith('/api/v1/transfers', { signal, cache: 'no-store' });
});

it.each(['start', 'pause', 'resume', 'cancel'] as const)('posts explicit %s with empty JSON', async action => {
 const mock = vi.fn().mockResolvedValue(response(status)); vi.stubGlobal('fetch', mock);
 const signal = new AbortController().signal;
 await expect(mutateTransfer(status.object_id, action, signal)).resolves.toEqual(status);
 expect(mock).toHaveBeenCalledWith('/api/v1/media/' + status.object_id + '/transfer/' + action, {
  method: 'POST', body: '{}', headers: { 'Content-Type': 'application/json' }, signal, cache: 'no-store',
 });
});
it.each([[503,'catalog_unavailable'],[409,'media_removed'],[404,'media_not_found'],[503,'local_media_unavailable'],[503,'transfer_unavailable'],[409,'transfer_conflict'],[404,'transfer_not_found'],[400,'invalid_request']])('classifies %s %s', async (code, kind) => {
 vi.stubGlobal('fetch', vi.fn().mockImplementation(() => Promise.resolve(response({ error: kind }, code as number))));
 await expect(fetchTransfers(new AbortController().signal)).rejects.toMatchObject({ kind });
 await expect(mutateTransfer(status.object_id, 'start', new AbortController().signal)).rejects.toMatchObject({ kind });
});
it('uses mode-neutral errors because the endpoint serves real and mock transfers', async () => {
 vi.stubGlobal('fetch', vi.fn().mockResolvedValue(response({ error: 'transfer_unavailable' }, 503)));
 await expect(fetchTransfers(new AbortController().signal)).rejects.toMatchObject({
  kind: 'transfer_unavailable', message: 'Transfer service unavailable.',
 });
});
it('rejects mismatched mutation identity', async () => {
 vi.stubGlobal('fetch', vi.fn().mockResolvedValue(response({ ...status, object_id: 'media_' + 'b'.repeat(64) })));
 await expect(mutateTransfer(status.object_id, 'start', new AbortController().signal)).rejects.toMatchObject({ kind: 'invalid-payload' });
});
it('rejects invalid action and ID before sending a request', async () => {
 const mock = vi.fn(); vi.stubGlobal('fetch', mock);
 await expect(mutateTransfer('../bad', 'start', new AbortController().signal)).rejects.toMatchObject({ kind: 'invalid-payload' });
 // @ts-expect-error Runtime input must also be validated.
 await expect(mutateTransfer(status.object_id, 'restart', new AbortController().signal)).rejects.toMatchObject({ kind: 'invalid-payload' });
 expect(mock).not.toHaveBeenCalled();
});
it.each([[500, 'transfer_unavailable'], [503, '<script>']])('does not trust unknown or mismatched error %s %s', async (code, error) => {
 vi.stubGlobal('fetch', vi.fn().mockResolvedValue(response({ error }, code as number)));
 await expect(fetchTransfers(new AbortController().signal)).rejects.toMatchObject({ kind: 'http-error' });
});
