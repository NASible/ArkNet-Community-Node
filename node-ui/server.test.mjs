// @vitest-environment node
import { execFileSync } from 'node:child_process';
import fs from 'node:fs';
import http from 'node:http';
import https from 'node:https';
import os from 'node:os';
import path from 'node:path';
import tls from 'node:tls';
import { afterAll, beforeAll, expect, it } from 'vitest';
import { dropPrivileges, readTLS, startHttps } from './server.mjs';

let dir, upstream, front, seen = [];
const listen = (server, port = 0) => new Promise(resolve => server.listen(port, '127.0.0.1', () => resolve(server.address().port)));

beforeAll(async () => {
  dir = fs.mkdtempSync(path.join(os.tmpdir(), 'arknet-ui-tls-'));
  const key = path.join(dir, 'key.pem'), cert = path.join(dir, 'cert.pem');
  execFileSync('openssl', ['ecparam', '-name', 'prime256v1', '-genkey', '-noout', '-out', key]);
  execFileSync('openssl', ['req', '-new', '-x509', '-key', key, '-out', cert, '-days', '1', '-subj', '/CN=localhost']);
  upstream = http.createServer((req, res) => {
    seen.push({ method: req.method, url: req.url, headers: req.headers });
    res.writeHead(200, { 'content-type': 'text/plain', 'set-cookie': '__Host-arknet_operator=x; Secure; HttpOnly; SameSite=Strict; Path=/' });
    req.pipe(res);
  });
  const upstreamPort = await listen(upstream);
  front = await startHttps({ ...readTLS(cert, key), port: 0, upstreamPort, host: '127.0.0.1' });
});
afterAll(() => { front?.close(); upstream?.close(); fs.rmSync(dir, { recursive: true, force: true }); });

const call = (options, body) => new Promise((resolve, reject) => {
  const req = https.request({ host: '127.0.0.1', port: front.address().port, rejectUnauthorized: false, ...options }, res => {
    let data = ''; res.on('data', c => { data += c; }); res.on('end', () => resolve({ res, data }));
  });
  req.on('error', reject); req.end(body);
});

it('forwards to the UI as loopback, keeps cookies and adds security headers', async () => {
  const { res, data } = await call({ method: 'POST', path: '/api/v1/operator/sessions', headers: { host: '192.168.1.20:8443', origin: 'https://192.168.1.20:8443' } }, '{"x":1}');
  expect(res.statusCode).toBe(200);
  expect(data).toBe('{"x":1}');
  expect(res.headers['set-cookie']?.[0]).toContain('__Host-arknet_operator=x');
  expect(res.headers['x-content-type-options']).toBe('nosniff');
  expect(res.headers['x-frame-options']).toBe('DENY');
  expect(res.headers['strict-transport-security']).toBeUndefined();
  const last = seen.at(-1);
  expect(last.headers.host).toBe('127.0.0.1:5173');
  expect(last.headers['x-forwarded-host']).toBe('192.168.1.20:8443');
  expect(last.headers['x-forwarded-proto']).toBe('https');
  expect(last.headers.origin).toBe('https://192.168.1.20:8443');
});

it('refuses TLS below 1.2', async () => {
  const outcome = await new Promise(resolve => {
    const socket = tls.connect({ host: '127.0.0.1', port: front.address().port, rejectUnauthorized: false, maxVersion: 'TLSv1.1', minVersion: 'TLSv1' },
      () => { socket.end(); resolve('connected'); });
    socket.on('error', () => resolve('refused'));
  });
  expect(outcome).toBe('refused');
});

it('drops root privileges and clears supplementary groups', () => {
  const calls = [];
  const runtime = {
    getuid: () => 0,
    setgroups: groups => calls.push(['groups', groups]),
    setgid: id => calls.push(['gid', id]),
    setuid: id => calls.push(['uid', id]),
  };
  dropPrivileges(runtime, 1000, 1000);
  expect(calls).toEqual([['groups', []], ['gid', 1000], ['uid', 1000]]);
});

it('keeps every pre-drop module root-owned and non-writable by node', () => {
  const dockerfile = fs.readFileSync(path.join(import.meta.dirname, 'Dockerfile'), 'utf8');
  expect(dockerfile).toContain('RUN chown -R root:root /app && chmod -R go-w /app');
  expect(dockerfile.indexOf('RUN chown -R root:root /app')).toBeLessThan(dockerfile.indexOf('CMD ["sh", "./start-ui.sh"]'));
});

it('serves no HTTPS listener without a certificate', () => {
  expect(readTLS(path.join(dir, 'missing.pem'), path.join(dir, 'missing.key'))).toBeNull();
});
