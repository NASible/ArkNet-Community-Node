// @vitest-environment node
import { execFileSync } from 'node:child_process';
import fs from 'node:fs';
import http from 'node:http';
import https from 'node:https';
import os from 'node:os';
import path from 'node:path';
import tls from 'node:tls';
import { afterAll, beforeAll, expect, it } from 'vitest';
import { PROXY_ERROR, createHandler, dropPrivileges, readTLS, startHttp, startHttps, upstreamFor } from './server.mjs';

let dir, staticDir, agent, member, front, plain, seen = [], logged = [];
const listen = (server, port = 0) => new Promise(resolve => server.listen(port, '127.0.0.1', () => resolve(server.address().port)));
const recorder = name => http.createServer((req, res) => {
  let body = '';
  req.on('data', c => { body += c; });
  req.on('end', () => {
    seen.push({ upstream: name, method: req.method, url: req.url, headers: req.headers, body });
    res.writeHead(200, { 'content-type': 'text/plain', 'keep-alive': 'timeout=99',
      'set-cookie': '__Host-arknet_operator=x; Secure; HttpOnly; SameSite=Strict; Path=/' });
    res.end(body);
  });
});

beforeAll(async () => {
  dir = fs.mkdtempSync(path.join(os.tmpdir(), 'arknet-ui-tls-'));
  const key = path.join(dir, 'key.pem'), cert = path.join(dir, 'cert.pem');
  execFileSync('openssl', ['ecparam', '-name', 'prime256v1', '-genkey', '-noout', '-out', key]);
  execFileSync('openssl', ['req', '-new', '-x509', '-key', key, '-out', cert, '-days', '1', '-subj', '/CN=localhost']);
  staticDir = path.join(dir, 'dist');
  fs.mkdirSync(path.join(staticDir, 'assets'), { recursive: true });
  fs.writeFileSync(path.join(staticDir, 'index.html'), '<!doctype html><script type="module" src="/assets/app-1a2b.js"></script>');
  fs.writeFileSync(path.join(staticDir, 'assets/app-1a2b.js'), 'console.log("app")');
  fs.writeFileSync(path.join(staticDir, 'assets/app-1a2b.js.map'), '{"sources":["src/main.tsx"]}');
  fs.writeFileSync(path.join(dir, 'secret.txt'), 'outside the build');
  agent = recorder('agent');
  member = recorder('member');
  const handler = createHandler({ staticDir, agentUrl: `http://127.0.0.1:${await listen(agent)}`,
    memberUrl: `http://127.0.0.1:${await listen(member)}`, log: message => logged.push(message) });
  front = await startHttps({ ...readTLS(cert, key), handler, port: 0, host: '127.0.0.1' });
  plain = await startHttp({ handler, port: 0, host: '127.0.0.1' });
});
afterAll(() => { front?.close(); plain?.close(); agent?.close(); member?.close(); fs.rmSync(dir, { recursive: true, force: true }); });

const call = (options, body) => new Promise((resolve, reject) => {
  const req = https.request({ host: '127.0.0.1', port: front.address().port, rejectUnauthorized: false, ...options }, res => {
    let data = ''; res.on('data', c => { data += c; }); res.on('end', () => resolve({ res, data }));
  });
  req.on('error', reject); req.end(body);
});
const get = (target, method = 'GET', headers = {}, body) => new Promise((resolve, reject) => {
  const req = http.request({ host: '127.0.0.1', port: plain.address().port, path: target, method, headers }, res => {
    let data = ''; res.on('data', c => { data += c; }); res.on('end', () => resolve({ status: res.statusCode, headers: res.headers, data }));
  });
  req.on('error', reject); req.end(body);
});

it('forwards HTTPS requests to the Agent as the loopback UI, keeps cookies and adds security headers', async () => {
  const { res, data } = await call({ method: 'POST', path: '/api/v1/operator/sessions', headers: { host: '192.168.1.20:8443', origin: 'https://192.168.1.20:8443' } }, '{"x":1}');
  expect(res.statusCode).toBe(200);
  expect(data).toBe('{"x":1}');
  expect(res.headers['set-cookie']?.[0]).toContain('__Host-arknet_operator=x');
  expect(res.headers['x-content-type-options']).toBe('nosniff');
  expect(res.headers['x-frame-options']).toBe('DENY');
  expect(res.headers['strict-transport-security']).toBeUndefined();
  const last = seen.at(-1);
  expect(last.upstream).toBe('member');
  expect(last.headers.host).toBe('127.0.0.1:5173');
  expect(last.headers['x-forwarded-host']).toBe('192.168.1.20:8443');
  expect(last.headers['x-forwarded-proto']).toBe('https');
  expect(last.headers.origin).toBe('https://192.168.1.20:8443');
});

it('routes member and public routes to the member listener and all other /api to management', () => {
  for (const route of ['/api/v1/operator/setup', '/api/v1/operator/setup/confirm', '/api/v1/operator/sessions',
    '/api/v1/local-users/sessions', '/api/v1/global-users/sessions', '/api/v1/global-users/login',
    '/api/v1/global-users/login/complete', '/api/v1/local-users/session', '/api/v1/global-users/session']) {
    expect(upstreamFor(route), route).toBe('member');
  }
  for (const route of ['/api/v1/operator/session', '/api/v1/status', '/api/v1/local-users', '/api/v1/local-users/session/x',
    '/api/web-seed/v1/media/x', '/api']) {
    expect(upstreamFor(route), route).toBe('agent');
  }
  for (const route of ['/', '/member', '/login/callback', '/apix', '/assets/app.js']) expect(upstreamFor(route), route).toBeNull();
});

it('forwards plain-HTTP API calls unchanged: exact path and query, method, body and headers', async () => {
  const target = '/api/web-seed/v1/media/media_abc/GRANT%2Fsegment?grant=Q&x=%20';
  const response = await get(target, 'PUT', { authorization: 'Bearer t', cookie: 'c=1', 'content-type': 'application/json' }, '{"a":1}');
  expect(response.status).toBe(200);
  expect(response.data).toBe('{"a":1}');
  expect(response.headers['keep-alive']).not.toBe('timeout=99'); // hop-by-hop: not forwarded
  expect(seen.at(-1)).toMatchObject({ upstream: 'agent', method: 'PUT', url: target, body: '{"a":1}',
    headers: { authorization: 'Bearer t', cookie: 'c=1', host: `127.0.0.1:${plain.address().port}` } });
  expect(seen.at(-1).headers['x-forwarded-proto']).toBeUndefined();
});

it('serves the built app shell for client routes and immutable hashed assets, with a CSP', async () => {
  for (const route of ['/', '/member', '/login/callback?x=1']) {
    const page = await get(route);
    expect(page.status, route).toBe(200);
    expect(page.data).toContain('/assets/app-1a2b.js');
    expect(page.headers['cache-control']).toBe('no-cache');
    expect(page.headers['content-security-policy']).toContain("script-src 'self'");
    expect(page.headers['x-frame-options']).toBe('DENY');
  }
  const asset = await get('/assets/app-1a2b.js');
  expect(asset.status).toBe(200);
  expect(asset.headers['content-type']).toBe('text/javascript; charset=utf-8');
  expect(asset.headers['cache-control']).toContain('immutable');
  const head = await get('/assets/app-1a2b.js', 'HEAD');
  expect([head.status, head.data, head.headers['content-length']]).toEqual([200, '', String('console.log("app")'.length)]);
});

it('never serves source maps, missing assets or files outside the build', async () => {
  expect((await get('/assets/app-1a2b.js.map')).status).toBe(404);
  expect((await get('/assets/missing.js')).status).toBe(404);
  for (const route of ['/../secret.txt', '/%2e%2e/secret.txt', '/..%2fsecret.txt', '/assets/..%2f..%2fsecret.txt', '/%E0%A4%A']) {
    const response = await get(route);
    expect(response.data, route).not.toContain('outside the build');
  }
  expect((await get('/', 'POST')).status).toBe(405);
});

it('answers 502 with a constant, URL-free diagnostic when the Agent is unreachable', async () => {
  const unreachable = createHandler({ staticDir, agentUrl: 'http://127.0.0.1:1', memberUrl: 'http://127.0.0.1:1',
    log: message => logged.push(message) });
  const server = await startHttp({ handler: unreachable, port: 0, host: '127.0.0.1' });
  try {
    const status = await new Promise((resolve, reject) => {
      http.request({ host: '127.0.0.1', port: server.address().port, path: '/api/torrent/v1/media/m/GRANT_SENTINEL?g=QUERY_SENTINEL' },
        res => { res.resume(); resolve(res.statusCode); }).on('error', reject).end();
    });
    expect(status).toBe(502);
  } finally { server.close(); }
  expect(logged.at(-1)).toBe(PROXY_ERROR);
  expect(logged.join('\n')).not.toMatch(/SENTINEL|torrent/);
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

it('keeps every pre-drop module root-owned and ships only the build in the runtime stage', () => {
  const dockerfile = fs.readFileSync(path.join(import.meta.dirname, 'Dockerfile'), 'utf8');
  const runtime = dockerfile.slice(dockerfile.lastIndexOf('\nFROM '));
  expect(runtime).toContain('RUN chown -R root:root /app && chmod -R go-w /app');
  expect(runtime.indexOf('RUN chown -R root:root /app')).toBeLessThan(runtime.indexOf('CMD ["sh", "./start-ui.sh"]'));
  expect(runtime).not.toMatch(/npm (ci|install)|COPY[^\n]*(src|node_modules|vite\.config)/);
});

it('refuses to start without a build', () => {
  expect(() => createHandler({ staticDir: path.join(dir, 'missing'), agentUrl: 'http://a', memberUrl: 'http://b' })).toThrow();
});

it('serves no HTTPS listener without a certificate', () => {
  expect(readTLS(path.join(dir, 'missing.pem'), path.join(dir, 'missing.key'))).toBeNull();
});
