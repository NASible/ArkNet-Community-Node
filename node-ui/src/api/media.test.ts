import { afterEach, expect, it, vi } from 'vitest';
import { fetchMedia, selectMedia } from './media';

const object = { id: `media_${'a'.repeat(64)}`, title: 'A film', size: 1024, state: 'ACTIVE', local_state: 'NOT_PRESENT' };
const catalog = { objects: [object], allocation_bytes: 4096, reserved_bytes: 0, core_available: true };
const response = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
afterEach(() => vi.unstubAllGlobals());

it('fetches the same-origin catalog without caching and forwards cancellation', async () => {
  const fetchMock = vi.fn().mockResolvedValue(response(catalog));
  vi.stubGlobal('fetch', fetchMock);
  const signal = new AbortController().signal;
  await expect(fetchMedia(signal)).resolves.toEqual(catalog);
  expect(fetchMock).toHaveBeenCalledWith('/api/v1/media', { signal, cache: 'no-store' });
});

it('accepts -1 only as the unlimited allocation sentinel without converting byte counts', async () => {
  const unlimited = { ...catalog, allocation_bytes: -1, reserved_bytes: 8192 };
  vi.stubGlobal('fetch', vi.fn().mockResolvedValue(response(unlimited)));
  await expect(fetchMedia(new AbortController().signal)).resolves.toEqual(unlimited);
});

it.each([
  { ...catalog, allocation_bytes: -2 },
  { ...catalog, allocation_bytes: -1.5 },
  { ...catalog, allocation_bytes: '-1' },
  { ...catalog, reserved_bytes: -1 },
  { ...catalog, reserved_bytes: 1.5 },
  { ...catalog, allocation_bytes: Number.MAX_SAFE_INTEGER + 1 },
  { ...catalog, core_available: 'true' },
  { ...catalog, objects: [{ ...object, id: ' ' }] },
  { ...catalog, objects: [{ ...object, title: 123 }] },
  { ...catalog, objects: [{ ...object, size: -1 }] },
  { ...catalog, objects: [{ ...object, size: 1.5 }] },
  { ...catalog, objects: [{ ...object, state: 'UNKNOWN' }] },
  { ...catalog, objects: [{ ...object, local_state: 'UNKNOWN' }] },
  { ...catalog, objects: [object, object] },
  null,
])('rejects invalid catalog payload %#', async body => {
  vi.stubGlobal('fetch', vi.fn().mockResolvedValue(response(body)));
  await expect(fetchMedia(new AbortController().signal)).rejects.toMatchObject({ kind: 'invalid-payload' });
});
it('rejects malformed JSON', async () => {
  vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response('not json')));
  await expect(fetchMedia(new AbortController().signal)).rejects.toMatchObject({ kind: 'invalid-payload' });
});

it('posts an encoded opaque ID with empty JSON and no caching', async () => {
  const queued = { ...object, local_state: 'QUEUED' };
  const fetchMock = vi.fn().mockResolvedValue(response(queued));
  vi.stubGlobal('fetch', fetchMock);
  const signal = new AbortController().signal;
  await expect(selectMedia(object.id, signal)).resolves.toEqual(queued);
  expect(fetchMock).toHaveBeenCalledWith(`/api/v1/media/${object.id}/selection`, {
    method: 'POST', body: '{}', headers: { 'Content-Type': 'application/json' }, signal, cache: 'no-store',
  });
});
it.each([
  [409, 'storage_allocation_exceeded'], [409, 'media_removed'], [404, 'media_not_found'],
  [503, 'catalog_unavailable'], [503, 'local_media_unavailable'], [400, 'invalid_request'],
])('classifies HTTP %s %s', async (status, kind) => {
  vi.stubGlobal('fetch', vi.fn().mockImplementation(() => Promise.resolve(response({ error: kind }, status))));
  await expect(selectMedia(object.id, new AbortController().signal)).rejects.toMatchObject({ kind });
  await expect(fetchMedia(new AbortController().signal)).rejects.toMatchObject({ kind });
});
it('does not trust an unknown error response', async () => {
  vi.stubGlobal('fetch', vi.fn().mockResolvedValue(response({ error: '<script>' }, 500)));
  await expect(selectMedia(object.id, new AbortController().signal)).rejects.toMatchObject({ kind: 'http-error' });
});
it.each([{ ...object, id: 'different' }, { ...object, local_state: 'NOT_PRESENT' }, { ...object, size: -1 }])('rejects invalid mutation confirmation %#', async body => {
  vi.stubGlobal('fetch', vi.fn().mockResolvedValue(response(body)));
  await expect(selectMedia(object.id, new AbortController().signal)).rejects.toMatchObject({ kind: 'invalid-payload' });
});

it.each(['QUEUED', 'DOWNLOADING', 'AVAILABLE', 'ERROR'])('accepts an idempotent saved selection in %s', async local_state => {
 const saved = { ...object, local_state };
 vi.stubGlobal('fetch', vi.fn().mockResolvedValue(response(saved)));
 await expect(selectMedia(object.id, new AbortController().signal)).resolves.toEqual(saved);
});

it('accepts a schema-valid empty title', async () => {
 const body = { ...catalog, objects: [{ ...object, title: '' }] };
 vi.stubGlobal('fetch', vi.fn().mockResolvedValue(response(body)));
 await expect(fetchMedia(new AbortController().signal)).resolves.toEqual(body);
});
it.each(['../selection', 'media_short', `media_${'A'.repeat(64)}`, `media_${'a'.repeat(63)}`])('rejects a malformed opaque media ID %s', async id => {
 vi.stubGlobal('fetch', vi.fn().mockResolvedValue(response({ ...catalog, objects: [{ ...object, id }] })));
 await expect(fetchMedia(new AbortController().signal)).rejects.toMatchObject({ kind: 'invalid-payload' });
});

it('rejects an invalid selection ID before any request', async () => {
 const fetchMock = vi.fn();
 vi.stubGlobal('fetch', fetchMock);
 await expect(selectMedia('../selection', new AbortController().signal)).rejects.toMatchObject({ kind: 'invalid-payload' });
 expect(fetchMock).not.toHaveBeenCalled();
});
