import { afterEach, expect, it, vi } from 'vitest';
import { fetchPeerServing, updatePeerServing } from './settings';

afterEach(() => vi.unstubAllGlobals());

it('loads and updates peer serving through the local Agent API', async () => {
  const fetchMock = vi.fn()
    .mockResolvedValueOnce(new Response('{"enabled":false}', { status: 200, headers: { 'Content-Type': 'application/json' } }))
    .mockResolvedValueOnce(new Response('{"enabled":true}', { status: 200, headers: { 'Content-Type': 'application/json' } }));
  vi.stubGlobal('fetch', fetchMock);
  await expect(fetchPeerServing()).resolves.toEqual({ enabled: false });
  await expect(updatePeerServing(true)).resolves.toEqual({ enabled: true });
  expect(fetchMock).toHaveBeenNthCalledWith(1, '/api/v1/settings/peer-serving', expect.objectContaining({ signal: undefined }));
  expect(fetchMock).toHaveBeenNthCalledWith(2, '/api/v1/settings/peer-serving', expect.objectContaining({ method: 'PUT', body: '{"enabled":true}' }));
});

it('rejects malformed setting responses', async () => {
  vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response('{"enabled":"true"}', { status: 200 })));
  await expect(fetchPeerServing()).rejects.toThrow(/invalid response/i);
});
