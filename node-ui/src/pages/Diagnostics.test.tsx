import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { Diagnostics as Report } from '../api/diagnostics';
import Diagnostics from './Diagnostics';
import { findings, headline } from './diagnosticsText';

const slow = { timeout: 4000 };
const healthy: Report = {
  generated_at: '2026-10-25T20:00:00Z',
  version: { agent: '0.1.0', release: { version: '0.1.0', channel: 'stable' } },
  agent: { status: 'healthy' },
  database: { status: 'healthy' },
  jellyfin: { status: 'healthy' },
  core: { status: 'ok', host: 'core.arknet.example', node_state: 'CONNECTED', enrollment_state: 'APPROVED', connectivity: 'CONNECTED', last_contact_at: '2026-10-25T19:59:30Z' },
  tracker: { status: 'ok', host: 'tracker.arknet.example' },
  tailscale: { status: 'not_detected' },
  storage: { status: 'healthy', kind: 'media', total_bytes: 500 * 2 ** 30, free_bytes: 200 * 2 ** 30 },
  media: { status: 'healthy', allocation_bytes: 100 * 2 ** 30, reserved_bytes: 20 * 2 ** 30, selections: 3 },
  transfers: { status: 'healthy', active: 1, paused: 0, failed: 0, completed: 4 },
};
const now = new Date('2026-10-25T20:00:00Z');
const with_ = (patch: Partial<Report>): Report => ({ ...healthy, ...patch });
const find = (r: Report, id: string) => findings(r, now).find(f => f.id === id)!;

afterEach(() => { cleanup(); vi.unstubAllGlobals(); });

describe('diagnostic explanations', () => {
  it('reports every required check', () => {
    expect(findings(healthy, now).map(f => f.label)).toEqual([
      'ArkNet version', 'ArkNet Agent', 'PostgreSQL', 'Jellyfin', 'Core connectivity', 'Private tracker',
      'Tailscale', 'Storage capacity', 'ArkNet media storage', 'Active transfers']);
    expect(headline(healthy).title).toBe('All Systems Operational.');
  });

  it('says federation is offline but local services keep working', () => {
    const r = with_({ core: { ...healthy.core, status: 'timeout', node_state: 'OFFLINE', last_contact_at: '2026-10-25T19:20:00Z' } });
    expect(headline(r).title).toBe('Federation Offline — Local services are still operating.');
    const core = find(r, 'core');
    expect(core.tone).toBe('error');
    expect(core.explanation).toContain('core.arknet.example did not respond');
    expect(core.explanation).toContain('Last successful contact: 40 minutes ago');
    expect(core.explanation).toContain('Local services keep running');
    expect(core.action).toContain('firewall');
  });

  it.each([
    ['dns', 'could not be found', 'DNS'],
    ['refused', 'refused the connection', 'port'],
    ['unreachable', 'no network route', 'router'],
    ['tls', 'certificate could not be verified', 'clock'],
    ['server_error', 'reported an internal problem (HTTP 503)', 'No action is needed'],
  ] as const)('explains a %s Core failure in plain language', (status, explanation, action) => {
    const core = find(with_({ core: { ...healthy.core, status, http_status: status === 'server_error' ? 503 : undefined } }), 'core');
    expect(core.explanation).toContain(explanation);
    expect(core.action).toContain(action);
    expect(core.explanation).not.toMatch(/ECONNREFUSED|dial tcp|x509|context deadline/i);
  });

  it('describes contact history accurately', () => {
    const pendingDown = find(with_({ core: { ...healthy.core, status: 'refused', node_state: 'ENROLLMENT_PENDING', last_contact_at: null } }), 'core');
    expect(pendingDown.explanation).toContain('Enrollment was requested earlier; no heartbeat has been recorded yet.');
    expect(pendingDown.explanation).not.toContain('never');
    const unknown = find(with_({ database: { status: 'unhealthy' }, core: { ...healthy.core, node_state: 'UNKNOWN' } }), 'core');
    expect(unknown.summary).toBe('Reachable; Node state unknown');
    expect(unknown.tone).toBe('warning');
  });

  it('distinguishes a stopped database and media server', () => {
    const db = with_({ database: { status: 'unhealthy' } });
    expect(headline(db).title).toMatch(/^Local Database Unavailable/);
    expect(find(db, 'database').action).toContain('re-run the installer');
    const jf = with_({ jellyfin: { status: 'unhealthy' } });
    expect(headline(jf).title).toBe('Media Server Offline — members cannot stream from this Node.');
    expect(headline(jf).detail).toBe('Federation and the Agent are still operating.');
  });

  it('explains enrollment, revocation and unconfigured Core', () => {
    const pending = with_({ core: { ...healthy.core, node_state: 'ENROLLMENT_PENDING', last_contact_at: null } });
    expect(headline(pending).title).toMatch(/^Waiting for Approval/);
    expect(find(pending, 'core').action).toContain('approve this Node');
    expect(headline(with_({ core: { ...healthy.core, node_state: 'REVOKED' } })).title).toMatch(/^Node Access Revoked/);
    expect(find(with_({ core: { ...healthy.core, status: 'not_configured', node_state: 'PROVISIONING' } }), 'core').summary).toBe('Not configured');
  });

  it('warns about tracker, disk, allocation and transfer problems', () => {
    const tracker = find(with_({ tracker: { status: 'dns', host: 'tracker.arknet.example' } }), 'tracker');
    expect(tracker.tone).toBe('warning');
    expect(tracker.explanation).toContain("continue from Core's web seed");
    const disk = find(with_({ storage: { ...healthy.storage, free_bytes: 10 * 2 ** 30 } }), 'storage');
    expect(disk.tone).toBe('error');
    expect(disk.summary).toBe('10 GiB free of 500 GiB');
    expect(find(with_({ media: { ...healthy.media, allocation_bytes: 0 } }), 'media').summary).toBe('Disabled (0 GiB allocated)');
    expect(find(with_({ media: { ...healthy.media, reserved_bytes: 99 * 2 ** 30 } }), 'media').tone).toBe('warning');
    expect(find(with_({ transfers: { ...healthy.transfers, status: 'unavailable' } }), 'transfers').summary).toBe('Transfer service not responding');
    expect(find(with_({ transfers: { ...healthy.transfers, failed: 2 } }), 'transfers').summary).toBe('1 downloading, 0 paused, 2 failed');
    expect(headline(with_({ transfers: { ...healthy.transfers, failed: 2 } })).title).toBe('1 item needs attention.');
  });

  it('is informational about Tailscale and the release', () => {
    expect(find(healthy, 'tailscale').explanation).toContain('reaches Core directly');
    expect(find(with_({ tailscale: { status: 'detected' } }), 'tailscale').tone).toBe('info');
    const noRelease = find(with_({ version: { agent: '0.0.1-dev', release: null } }), 'version');
    expect(noRelease.summary).toBe('Agent build 0.0.1-dev');
    expect(noRelease.tone).toBe('warning');
  });
});

describe('Diagnostics page', () => {
  it('renders the headline and every check from the Agent', async () => {
    const r = with_({ core: { ...healthy.core, status: 'refused' } });
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response(JSON.stringify(r))));
    render(<Diagnostics />);
    expect((await screen.findByRole('alert', undefined, slow)).textContent).toContain('Federation Offline — Local services are still operating.');
    const list = screen.getByRole('list', { name: 'Diagnostic checks' });
    expect(within(list).getAllByRole('listitem')).toHaveLength(10);
    const core = list.querySelector('[data-check="core"]') as HTMLElement;
    expect(core.textContent).toContain('Federation offline — connection refused');
    expect(core.textContent).toContain('What to do:');
  });

  it('explains when the Agent itself is unreachable and can re-check', async () => {
    const fetchMock = vi.fn().mockResolvedValueOnce(new Response('Bad Gateway', { status: 502 }))
      .mockResolvedValueOnce(new Response(JSON.stringify(healthy)));
    vi.stubGlobal('fetch', fetchMock);
    render(<Diagnostics />);
    expect((await screen.findByRole('alert', undefined, slow)).textContent).toContain('Node Agent Offline');
    fireEvent.click(screen.getByRole('button', { name: 'Run checks again' }));
    expect((await screen.findByRole('status', undefined, slow)).textContent).toContain('All Systems Operational.');
  });

  it('rejects malformed responses instead of guessing', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response(JSON.stringify({ ...healthy, core: { status: 'weird' } }))));
    render(<Diagnostics />);
    await waitFor(() => expect(screen.getByRole('alert').textContent).toContain('unexpected response'), slow);
  });
});
