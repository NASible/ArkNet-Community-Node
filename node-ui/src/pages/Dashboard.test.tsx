import { act, cleanup, fireEvent, render, screen, within } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { AgentStatus } from '../api/status';
import Dashboard from './Dashboard';

const status: AgentStatus = {
  node_id: 'node_01JTESTOPAQUE',
  version: '9.8.7-test',
  state: 'MESH',
  state_changed_at: '2026-10-10T18:15:30Z',
  enrollment_state: 'APPROVED',
  core_connectivity: 'CONNECTED',
  last_successful_core_contact_at: '2026-10-10T18:15:20Z',
  database_healthy: true,
  agent_healthy: true,
  jellyfin_healthy: null,
  release: { version: '9.8.7-test', channel: 'development' },
};
const response = (body: unknown, httpStatus = 200) => new Response(JSON.stringify(body), {
  status: httpStatus,
  headers: { 'Content-Type': 'application/json' },
});

const renderStatus = async (value = status) => {
  vi.stubGlobal('fetch', vi.fn().mockResolvedValue(response(value)));
  render(<Dashboard />);
  await screen.findByText(value.node_id);
};

afterEach(() => { cleanup(); vi.unstubAllGlobals(); vi.useRealTimers(); });

describe('Dashboard rendering', () => {
  it('shows loading before status arrives', () => {
    vi.stubGlobal('fetch', vi.fn(() => new Promise(() => {})));
    render(<Dashboard />);

    expect(screen.getByRole('status').textContent).toContain('Loading Node Agent status');
  });

  it('renders every status field with semantic local and federation regions', async () => {
    await renderStatus();

    expect(screen.getByRole('heading', { name: 'Node dashboard', level: 1 })).toBeTruthy();
    const local = screen.getByRole('region', { name: 'Local services' });
    const federation = screen.getByRole('region', { name: 'Federation' });
    expect(within(local).getByText('Running')).toBeTruthy();
    expect(within(federation).getByText('Connected')).toBeTruthy();
    expect(screen.getByText(status.node_id)).toBeTruthy();
    expect(screen.getByText(status.version)).toBeTruthy();
    expect(screen.getByText('Mesh connected', { selector: '[data-machine-value="MESH"]' })).toBeTruthy();
    expect(screen.getByText('Approved')).toBeTruthy();
    expect(screen.getAllByText('Healthy')).toHaveLength(2);

    const stateTime = screen.getByText('Oct 10, 2026, 6:15:30 PM UTC');
    expect(stateTime.closest('time')?.getAttribute('datetime')).toBe(status.state_changed_at);
    const contactTime = screen.getByText('Oct 10, 2026, 6:15:20 PM UTC');
    expect(contactTime.closest('time')?.getAttribute('datetime')).toBe(status.last_successful_core_contact_at);
  });

  it('shows the current ArkNet release version and channel from the manifest', async () => {
    await renderStatus({ ...status, version: '9.8.7-test', release: { version: '9.8.7-test', channel: 'development' } });
    expect(screen.getByText('ArkNet 9.8.7-test')).toBeTruthy();
    expect(screen.getByText('Development')).toBeTruthy();
    cleanup();
    await renderStatus({ ...status, version: '0.1.0', release: { version: '0.1.0', channel: 'stable' } });
    expect(screen.getByText('ArkNet 0.1.0')).toBeTruthy();
    expect(screen.getByText('0.1.0')).toBeTruthy();
    expect(screen.getByText('Stable')).toBeTruthy();
  });

  it('falls back to the Agent build and says the release manifest is unavailable', async () => {
    await renderStatus({ ...status, release: null });
    expect(screen.getByText('ArkNet 9.8.7-test')).toBeTruthy();
    expect(screen.getByText('Release manifest unavailable — showing Agent build 9.8.7-test')).toBeTruthy();
  });

  it('makes a full opaque Node ID available as selectable text without controls', async () => {
    await renderStatus();

    const nodeId = screen.getByText(status.node_id);
    expect(nodeId.classList.contains('node-id')).toBe(true);
    expect(screen.queryByRole('textbox')).toBeNull();
    expect(screen.queryByRole('button', { name: /configure|enroll|credential/i })).toBeNull();
  });

  it('states Never when Core has never been contacted', async () => {
    await renderStatus({ ...status, last_successful_core_contact_at: null });

    expect(screen.getByText('Never')).toBeTruthy();
  });

  it('distinguishes healthy local services from an offline federation', async () => {
    await renderStatus({ ...status, core_connectivity: 'OFFLINE' });

    const local = screen.getByRole('region', { name: 'Local services' });
    const federation = screen.getByRole('region', { name: 'Federation' });
    expect(within(local).getByText('Running')).toBeTruthy();
    expect(within(federation).getByText('Offline')).toBeTruthy();
    expect(within(federation).getByText(/local services are still available/i)).toBeTruthy();
  });

  it.each([
    ['PROVISIONING', 'Provisioning'],
    ['ENROLLMENT_PENDING', 'Enrollment pending'],
    ['CONNECTED', 'Connected'],
    ['SYNCHRONIZING', 'Synchronizing'],
    ['MESH', 'Mesh connected'],
    ['DEGRADED', 'Degraded'],
    ['OFFLINE', 'Offline'],
    ['RECONNECTING', 'Reconnecting'],
    ['QUARANTINED', 'Quarantined'],
    ['REVOKED', 'Revoked'],
    ['ERROR', 'Error'],
  ])('maps Node state %s to %s', async (machine, friendly) => {
    await renderStatus({ ...status, state: machine });
    expect(screen.getAllByText(friendly, { selector: `[data-machine-value="${machine}"]` }).length).toBeGreaterThan(0);
  });

  it.each([
    ['NOT_ENROLLED', 'Not enrolled'],
    ['PENDING', 'Pending approval'],
    ['APPROVED', 'Approved'],
    ['REJECTED', 'Rejected'],
  ])('maps enrollment %s to %s', async (machine, friendly) => {
    await renderStatus({ ...status, enrollment_state: machine });
    expect(screen.getByText(friendly)).toBeTruthy();
  });

  it.each([
    ['UNKNOWN', 'Unknown'],
    ['CONNECTED', 'Connected'],
    ['DEGRADED', 'Degraded'],
    ['OFFLINE', 'Offline'],
    ['RECONNECTING', 'Reconnecting'],
    ['NOT_IMPLEMENTED', 'Not implemented'],
  ])('maps Core connectivity %s to %s', async (machine, friendly) => {
    await renderStatus({ ...status, core_connectivity: machine });
    expect(screen.getByText(friendly, { selector: `[data-machine-value="${machine}"]` })).toBeTruthy();
  });

  it('shows an unknown future machine value safely and intelligibly', async () => {
    await renderStatus({ ...status, state: '<FUTURE_MODE>' });

    expect(screen.getByText('Unknown (<FUTURE_MODE>)')).toBeTruthy();
    expect(document.querySelector('future_mode')).toBeNull();
  });
});

describe('Dashboard operator errors', () => {
  it.each([
    [502, new Response('<h1>Bad Gateway</h1>', { status: 502 }), 'Node Agent is not responding', /Agent process.*proxy target/i],
    [503, response({ error: 'local state database unavailable' }, 503), 'Local database unavailable', /PostgreSQL.*database/i],
    ['invalid payload', response({ version: 'test' }), 'Status response is incompatible', /UI and Agent versions/i],
  ])('translates %s into actionable guidance', async (_case, failedResponse, title, guidance) => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(failedResponse));
    render(<Dashboard />);

    const alert = await screen.findByRole('alert');
    expect(within(alert).getByRole('heading', { name: title })).toBeTruthy();
    expect(alert.textContent).toMatch(guidance);
    expect(alert.textContent).not.toMatch(/Agent returned HTTP 50[23]/);
  });

  it('translates a network failure into actionable guidance and retries', async () => {
    const fetchMock = vi.fn()
      .mockRejectedValueOnce(new TypeError('Failed to fetch'))
      .mockResolvedValueOnce(response(status));
    vi.stubGlobal('fetch', fetchMock);
    render(<Dashboard />);

    const alert = await screen.findByRole('alert');
    expect(within(alert).getByRole('heading', { name: 'Cannot reach Node Agent' })).toBeTruthy();
    expect(alert.textContent).toMatch(/running.*local network.*proxy/i);
    fireEvent.click(screen.getByRole('button', { name: 'Retry status check' }));
    expect(await screen.findByText(status.node_id)).toBeTruthy();
  });

  it('turns a stalled request into actionable timeout feedback', async () => {
    vi.useFakeTimers();
    vi.stubGlobal('fetch', vi.fn((_url: string, options: RequestInit) => new Promise((_resolve, reject) => {
      options.signal?.addEventListener('abort', () => reject(new DOMException('Aborted', 'AbortError')), { once: true });
    })));
    render(<Dashboard />);

    await act(() => vi.advanceTimersByTimeAsync(10_000));
    const alert = screen.getByRole('alert');
    expect(within(alert).getByRole('heading', { name: 'Status request timed out' })).toBeTruthy();
    expect(alert.textContent).toMatch(/10 seconds.*Agent.*overloaded/i);
  });
});

describe('Dashboard polling', () => {
  it('polls sequentially five seconds after the prior request completes', async () => {
    vi.useFakeTimers();
    let resolveFirst!: (value: Response) => void;
    const fetchMock = vi.fn()
      .mockImplementationOnce(() => new Promise<Response>((resolve) => { resolveFirst = resolve; }))
      .mockResolvedValueOnce(response({ ...status, core_connectivity: 'RECONNECTING' }));
    vi.stubGlobal('fetch', fetchMock);
    render(<Dashboard />);

    await act(() => vi.advanceTimersByTimeAsync(5_000));
    expect(fetchMock).toHaveBeenCalledTimes(1);
    await act(async () => resolveFirst(response(status)));
    expect(screen.getByText(status.node_id)).toBeTruthy();
    await act(() => vi.advanceTimersByTimeAsync(4_999));
    expect(fetchMock).toHaveBeenCalledTimes(1);
    await act(() => vi.advanceTimersByTimeAsync(1));
    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(screen.queryByText('Loading Node Agent status…')).toBeNull();
  });

  it('keeps the last valid dashboard visible when a background refresh fails', async () => {
    vi.useFakeTimers();
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(response(status))
      .mockResolvedValueOnce(new Response('<h1>Bad Gateway</h1>', { status: 502 }));
    vi.stubGlobal('fetch', fetchMock);
    render(<Dashboard />);

    await act(async () => {});
    expect(screen.getByText(status.node_id)).toBeTruthy();
    await act(() => vi.advanceTimersByTimeAsync(5_000));
    await act(async () => {});

    expect(screen.getByText(status.node_id)).toBeTruthy();
    expect(within(screen.getByRole('alert')).getByText('Node Agent is not responding')).toBeTruthy();
  });

  it('stops polling and aborts an outstanding request after unmount', async () => {
    vi.useFakeTimers();
    const fetchMock = vi.fn(() => new Promise(() => {}));
    vi.stubGlobal('fetch', fetchMock);
    const { unmount } = render(<Dashboard />);
    await act(async () => {});
    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [, options] = fetchMock.mock.calls[0] as unknown as [string, RequestInit];

    unmount();
    expect(options.signal?.aborted).toBe(true);
    await act(() => vi.advanceTimersByTimeAsync(15_000));
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });
});
