import { afterEach, expect, it, vi } from 'vitest';
import { checkForUpdates, fetchUpdates, setAutomaticUpdates, UpdatesError } from './updates';

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
const valid = {
  current_version: '0.0.1-dev', channel: 'development', automatic_updates: true, automatic_install_supported: false,
  available: { version: '0.0.2-dev', channel: 'development', published_at: '2026-10-26T00:00:00Z' },
  discovery: { state: 'UPDATE_AVAILABLE', checked_at: '2026-10-25T20:00:00Z' },
};
afterEach(() => vi.unstubAllGlobals());

it('parses the exact update status', async () => {
  vi.stubGlobal('fetch', vi.fn().mockResolvedValue(json(valid)));
  expect(await fetchUpdates()).toEqual(valid);
});

it.each([
  { ...valid, discovery: { state: 'INSTALLING', checked_at: null } },
  { ...valid, channel: 'beta' },
  { ...valid, current_version: 'v1' },
  { ...valid, available: { ...valid.available, version: 'latest' } },
  { ...valid, available: { ...valid.available, url: 'https://x' } },
  { ...valid, automatic_updates: 'true' },
  { ...valid, extra: 1 },
])('rejects invalid status %#', async body => {
  vi.stubGlobal('fetch', vi.fn().mockResolvedValue(json(body)));
  await expect(fetchUpdates()).rejects.toBeInstanceOf(UpdatesError);
});

it('posts an exact empty check request and saves the setting', async () => {
  const fetchMock = vi.fn().mockResolvedValueOnce(json(valid)).mockResolvedValueOnce(json({ enabled: false })).mockResolvedValueOnce(json({ enabled: true }));
  vi.stubGlobal('fetch', fetchMock);
  await checkForUpdates();
  expect(fetchMock).toHaveBeenCalledWith('/api/v1/updates/check', expect.objectContaining({ method: 'POST', body: '{}' }));
  expect(await setAutomaticUpdates(false)).toBe(false);
  await expect(setAutomaticUpdates(false)).rejects.toBeInstanceOf(UpdatesError); // echo mismatch
});
