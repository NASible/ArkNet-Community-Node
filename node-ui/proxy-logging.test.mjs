// @vitest-environment node
import http from 'node:http';
import { inspect } from 'node:util';
import { createLogger, createServer, resolveConfig } from 'vite';
import { expect, it, vi } from 'vitest';

const listen = server => new Promise(resolve => server.listen(0, '127.0.0.1', () => resolve(server.address().port)));
const close = server => new Promise((resolve, reject) => server.close(error => error ? reject(error) : resolve()));
const request = (port, path, headers) => new Promise((resolve, reject) => {
  const req = http.get({ hostname: '127.0.0.1', port, path, headers, timeout: 5000 }, res => {
    let body = '';
    res.on('data', chunk => { body += chunk; });
    res.on('end', () => resolve({ status: res.statusCode, body }));
  });
  req.on('timeout', () => req.destroy(new Error('UI proxy test timed out')));
  req.on('error', reject);
});

it('redacts colored proxy messages and error metadata without suppressing build diagnostics', async () => {
  const logger = createLogger();
  const records = [];
  logger.error = (...args) => records.push(args);
  const config = await resolveConfig({
    configFile: new URL('./vite.config.ts', import.meta.url).pathname,
    customLogger: logger,
  }, 'build');
  const secret = 'ERROR_OBJECT_GRANT_SENTINEL_49B';
  const metadata = { timestamp: true, error: new Error(secret) };
  for (const kind of ['http proxy error', 'ws proxy error', 'ws proxy socket error']) {
    config.logger.error(`\u001b[31m${kind}: /api/${secret}\u001b[39m\n${secret}`, metadata);
    expect(records.at(-1)).toEqual([
      `${kind}: upstream request failed (details redacted)`, { timestamp: true },
    ]);
    expect(inspect(records.at(-1), { depth: null })).not.toContain(secret);
  }
  const buildError = new Error('unresolved application import');
  const buildOptions = { error: buildError };
  config.logger.error('build failed: unresolved application import', buildOptions);
  expect(records.at(-1)[0]).toBe('build failed: unresolved application import');
  expect(records.at(-1)[1]).toBe(buildOptions);
});

it('keeps grants intact when forwarding but excludes them from real failed-proxy diagnostics', async () => {
  const sentinels = ['PATH_GRANT_SENTINEL_49B', 'QUERY_GRANT_SENTINEL_49B', 'HEADER_GRANT_SENTINEL_49B'];
  const paths = [
    `/api/web-seed/v1/media/media_${'a'.repeat(64)}/${sentinels[0]}?grant=${sentinels[1]}`,
    `/api/torrent/v1/media/media_${'b'.repeat(64)}/${sentinels[0]}?grant=${sentinels[1]}`,
  ];
  const headers = { authorization: `Bearer ${sentinels[2]}`, cookie: `grant=${sentinels[2]}` };
  const seen = [];
  const backend = http.createServer((req, res) => {
    seen.push({ url: req.url, headers: req.headers });
    res.end('forwarded');
  });
  const backendPort = await listen(backend);
  const logs = [];
  const logger = createLogger();
  for (const level of ['info', 'warn', 'warnOnce', 'error']) {
    logger[level] = (...args) => logs.push(inspect(args, { depth: null }));
  }
  vi.stubEnv('ARKNET_AGENT_URL', `http://127.0.0.1:${backendPort}`);
  let vite;
  try {
    vite = await createServer({
      configFile: new URL('./vite.config.ts', import.meta.url).pathname,
      configLoader: 'runner', // Match server.mjs, not only Vite's bundled config loader.
      customLogger: logger,
      server: { host: '127.0.0.1', port: 0, strictPort: true, hmr: false, watch: null },
    });
    await vite.listen();
    const port = vite.httpServer.address().port;
    for (const path of paths) {
      expect(await request(port, path, headers)).toEqual({ status: 200, body: 'forwarded' });
      expect(seen.at(-1).url).toBe(path);
      expect(seen.at(-1).headers.authorization).toBe(headers.authorization);
      expect(seen.at(-1).headers.cookie).toBe(headers.cookie);
    }
    await close(backend);
    for (const path of paths) {
      logs.length = 0;
      expect((await request(port, path, headers)).status).toBe(502);
      const diagnostic = logs.join('\n');
      expect(diagnostic).toMatch(/proxy.*error|proxy.*failed/i);
      for (const sentinel of sentinels) expect(diagnostic).not.toContain(sentinel);
      expect(diagnostic).not.toContain('media_' + 'a'.repeat(64));
      expect(diagnostic).not.toContain('media_' + 'b'.repeat(64));
    }
  } finally {
    await vite?.close();
    if (backend.listening) await close(backend);
    vi.unstubAllEnvs();
  }
});
