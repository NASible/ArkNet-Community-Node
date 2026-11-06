// @vitest-environment node
import { spawn } from 'node:child_process';
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import http from 'node:http';
import { setTimeout as delay } from 'node:timers/promises';
import { expect, it } from 'vitest';

const listen = server => new Promise((resolve, reject) => {
  server.once('error', reject);
  server.listen(0, '127.0.0.1', () => resolve(server.address().port));
});
const close = server => new Promise(resolve => server.close(resolve));
const request = (path, method = 'GET', headers = {}, body = '') => new Promise((resolve, reject) => {
  const req = http.request({ hostname: '127.0.0.1', port: 5173, path, method, headers, timeout: 1000 }, res => {
    let data = '';
    res.on('data', chunk => { data += chunk; });
    res.on('end', () => resolve({ status: res.statusCode, body: data, headers: res.headers }));
  });
  req.on('timeout', () => req.destroy(new Error('UI request timed out')));
  req.on('error', reject);
  req.end(body);
});

// Exercise the actual package entrypoint and image command, not createServer
// with a custom logger: Vite debug and Node HTTP debug bypass that logger.
for (const entrypoint of ['dev', 'serve', 'preview', 'image']) {
  for (const debug of [
    { DEBUG: 'vite:proxy', NODE_DEBUG: '', NODE_DEBUG_NATIVE: '' },
    { DEBUG: '', NODE_DEBUG: 'http', NODE_DEBUG_NATIVE: 'http' },
  ]) {
    it(`${entrypoint} keeps inherited ${debug.DEBUG || 'NODE_DEBUG=http'} out of process output`, async () => {
      const sentinels = ['PATH_GRANT_SENTINEL_RUNTIME', 'QUERY_GRANT_SENTINEL_RUNTIME',
        'AUTH_GRANT_SENTINEL_RUNTIME', 'COOKIE_GRANT_SENTINEL_RUNTIME', 'BODY_GRANT_SENTINEL_RUNTIME'];
      const paths = ['web-seed', 'torrent'].map(kind =>
        `/api/${kind}/v1/media/media_${'a'.repeat(64)}/${sentinels[0]}?grant=${sentinels[1]}`);
      const body = JSON.stringify({ grant: sentinels[4] });
      const headers = { authorization: `Bearer ${sentinels[2]}`, cookie: `grant=${sentinels[3]}`,
        'content-type': 'application/json', 'content-length': Buffer.byteLength(body) };
      const seen = [];
      const backend = http.createServer((req, res) => {
        let data = '';
        req.on('data', chunk => { data += chunk; });
        req.on('end', () => {
          seen.push({ url: req.url, method: req.method, headers: req.headers, body: data });
          res.writeHead(201, { 'x-upstream-proof': 'intact' });
          res.end('forwarded intact');
        });
      });
      const backendPort = await listen(backend);
      const dockerfile = readFileSync(new URL('./Dockerfile', import.meta.url), 'utf8');
      // Preview gets a disposable static artifact so tests need no prior build.
      const previewDir = entrypoint === 'preview' ? mkdtempSync(path.join(os.tmpdir(), 'arknet-preview-')) : null;
      if (previewDir) writeFileSync(path.join(previewDir, 'index.html'), '<!doctype html><title>Preview fixture</title>');
      const command = entrypoint === 'image'
        ? JSON.parse(dockerfile.match(/^CMD (\[.*\])$/m)[1])
        : ['npm', 'run', entrypoint, ...(previewDir ? ['--', '--outDir', previewDir, '--port', '5173', '--strictPort'] : [])];
      const child = spawn(command[0], command.slice(1), {
        cwd: import.meta.dirname, detached: true,
        env: { ...process.env, ...debug, ARKNET_AGENT_URL: `http://127.0.0.1:${backendPort}`,
          ARKNET_UI_TLS_CERT: `${import.meta.filename}.missing-cert`, ARKNET_UI_HOST: '127.0.0.1',
          NO_COLOR: '1' }, stdio: ['ignore', 'pipe', 'pipe'],
      });
      let output = '';
      child.stdout.on('data', chunk => { output += chunk; });
      child.stderr.on('data', chunk => { output += chunk; });
      const exited = new Promise(resolve => child.once('close', resolve));
      try {
        const deadline = Date.now() + 15000;
        while (true) {
          if (child.exitCode !== null) throw new Error(`UI exited during startup:\n${output}`);
          try { if ((await request('/')).status === 200) break; } catch { /* Wait for listener. */ }
          if (Date.now() > deadline) throw new Error(`UI startup timed out:\n${output}`);
          await delay(50);
        }
        for (const path of paths) {
          const response = await request(path, 'POST', headers, body);
          expect(response.status).toBe(201);
          expect(response.body).toBe('forwarded intact');
          expect(response.headers['x-upstream-proof']).toBe('intact');
          expect(seen.at(-1)).toMatchObject({ url: path, method: 'POST', body,
            headers: { authorization: headers.authorization, cookie: headers.cookie } });
        }
        await close(backend);
        for (const path of paths) expect((await request(path, 'POST', headers, body)).status).toBe(502);
        await delay(100);
      } finally {
        try { process.kill(-child.pid, 'SIGTERM'); } catch { /* Already exited. */ }
        const stopped = await Promise.race([exited.then(() => true), delay(3000).then(() => false)]);
        if (!stopped) {
          try { process.kill(-child.pid, 'SIGKILL'); } catch { /* Already exited. */ }
          await exited;
        }
        if (backend.listening) await close(backend);
        if (previewDir) rmSync(previewDir, { recursive: true, force: true });
      }
      expect(output).toMatch(/Local:.*http:\/\/127\.0\.0\.1:5173/);
      expect(output).toContain('http proxy error: upstream request failed (details redacted)');
      for (const sentinel of sentinels) expect(output, 'raw child stdout/stderr must not contain grants').not.toContain(sentinel);
      expect(output).not.toContain(`media_${'a'.repeat(64)}`);
      expect(output).not.toMatch(/^HTTP \d+:/m);
    }, 25000);
  }
}
