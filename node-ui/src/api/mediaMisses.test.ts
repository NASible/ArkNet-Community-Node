import { afterEach, expect, it, vi } from 'vitest';
import { fetchMediaMisses, ignoreMediaMiss, MediaMissError, updateMediaMissPolicy } from './mediaMisses';

const id = `media_${'c'.repeat(64)}`;
const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
afterEach(() => vi.unstubAllGlobals());

it('parses exact notifications', async () => {
  vi.stubGlobal('fetch', vi.fn().mockResolvedValue(json({ policy: 'ASK', notifications: [{ media_id: id, title: 'T', size: 1, miss_count: 2 }] })));
  expect(await fetchMediaMisses()).toEqual({ policy: 'ASK', notifications: [{ media_id: id, title: 'T', size: 1, miss_count: 2 }] });
});

it.each([
  { policy: 'NEVER', notifications: [] },
  { policy: 'ASK' },
  { policy: 'ASK', notifications: [{ media_id: '../x', title: 'T', size: 1, miss_count: 1 }] },
  { policy: 'ASK', notifications: [{ media_id: id, title: 'T', size: 1, miss_count: 0 }] },
  { policy: 'ASK', notifications: [{ media_id: id, title: 'T', size: 1, miss_count: 1, member_id: 'member_x' }] },
])('rejects invalid or overbroad payloads %#', async body => {
  vi.stubGlobal('fetch', vi.fn().mockResolvedValue(json(body)));
  await expect(fetchMediaMisses()).rejects.toBeInstanceOf(MediaMissError);
});

it('ignores with an exact empty JSON body and requires 204', async () => {
  const fetchMock = vi.fn().mockResolvedValueOnce(new Response(null, { status: 204 })).mockResolvedValueOnce(json({ error: 'media_miss_not_found' }, 404));
  vi.stubGlobal('fetch', fetchMock);
  await ignoreMediaMiss(id);
  expect(fetchMock).toHaveBeenCalledWith(`/api/v1/media-misses/${id}/ignore`, expect.objectContaining({ method: 'POST', body: '{}' }));
  await expect(ignoreMediaMiss(id)).rejects.toBeInstanceOf(MediaMissError);
  await expect(ignoreMediaMiss('bad')).rejects.toBeInstanceOf(MediaMissError);
});

it('saves a policy and rejects a mismatched response', async () => {
  vi.stubGlobal('fetch', vi.fn().mockResolvedValueOnce(json({ policy: 'MANUAL' })).mockResolvedValueOnce(json({ policy: 'ask' })));
  expect(await updateMediaMissPolicy('MANUAL')).toBe('MANUAL');
  await expect(updateMediaMissPolicy('ASK')).rejects.toBeInstanceOf(MediaMissError);
});
