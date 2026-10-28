import { afterEach, describe, expect, it, vi } from 'vitest';
import { fetchAgentStatus, StatusFetchError } from './status';

const validStatus = {
  node_id: 'node_01JTESTOPAQUE',
  version: '1.4.2',
  state: 'RUNNING',
  state_changed_at: '2026-10-10T18:15:30Z',
  enrollment_state: 'APPROVED',
  core_connectivity: 'CONNECTED',
  last_successful_core_contact_at: '2026-10-10T18:15:20Z',
  database_healthy: true,
  agent_healthy: true,
  jellyfin_healthy: true,
  release: { version: '1.4.2', channel: 'stable' },
};

const jsonResponse = (body: unknown, status = 200) => new Response(JSON.stringify(body), {
  status,
  headers: { 'Content-Type': 'application/json' },
});

afterEach(() => vi.unstubAllGlobals());

describe('status API contract', () => {
  it('accepts and returns the complete healthy status contract', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(jsonResponse(validStatus)));

    await expect(fetchAgentStatus(new AbortController().signal)).resolves.toEqual(validStatus);
  });

  it('accepts null for a node that has never contacted Core', async () => {
    const neverContacted = { ...validStatus, last_successful_core_contact_at: null };
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(jsonResponse(neverContacted)));

    await expect(fetchAgentStatus(new AbortController().signal)).resolves.toEqual(neverContacted);
  });

  it.each([
    ['missing node ID', { ...validStatus, node_id: undefined }],
    ['empty version', { ...validStatus, version: ' ' }],
    ['invalid state timestamp', { ...validStatus, state_changed_at: 'yesterday' }],
    ['invalid contact timestamp', { ...validStatus, last_successful_core_contact_at: 'soon' }],
    ['non-boolean database health', { ...validStatus, database_healthy: 'yes' }],
    ['non-boolean Agent health', { ...validStatus, agent_healthy: 1 }],
    ['missing enrollment', { ...validStatus, enrollment_state: undefined }],
    ['unknown release channel', { ...validStatus, release: { version: '1.4.2', channel: 'beta' } }],
    ['non-semver release', { ...validStatus, release: { version: 'v1', channel: 'stable' } }],
    ['extra release field', { ...validStatus, release: { version: '1.4.2', channel: 'stable', components: {} } }],
    ['release not an object', { ...validStatus, release: '1.4.2' }],
  ])('rejects an invalid payload: %s', async (_case, body) => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(jsonResponse(body)));

    await expect(fetchAgentStatus(new AbortController().signal)).rejects.toMatchObject({
      kind: 'invalid-payload',
    });
  });

  it('treats a missing or null release as no manifest for older Agents', async () => {
    const { release: _release, ...older } = validStatus;
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(jsonResponse(older)));
    await expect(fetchAgentStatus(new AbortController().signal)).resolves.toEqual({ ...older, release: null });
  });

  it('classifies a proxy 502 response as Agent unavailable', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response('<h1>Bad Gateway</h1>', { status: 502 })));

    await expect(fetchAgentStatus(new AbortController().signal)).rejects.toEqual(
      expect.objectContaining<Partial<StatusFetchError>>({ kind: 'agent-unavailable' }),
    );
  });

  it('classifies the Agent database 503 response separately', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(jsonResponse(
      { error: 'local state database unavailable' },
      503,
    )));

    await expect(fetchAgentStatus(new AbortController().signal)).rejects.toEqual(
      expect.objectContaining<Partial<StatusFetchError>>({ kind: 'database-unavailable' }),
    );
  });

  it('classifies malformed JSON as an invalid payload', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response('not json', { status: 200 })));

    await expect(fetchAgentStatus(new AbortController().signal)).rejects.toEqual(
      expect.objectContaining<Partial<StatusFetchError>>({ kind: 'invalid-payload' }),
    );
  });
});
