// ArkNet Node management UI server (Prompt 45B).
//
// Vite serves the UI on 5173 (published on the host's loopback only, for SSH
// tunnels). When the Node's self-signed certificate is present, an HTTPS
// listener on 8443 serves the same UI to the LAN, so operator passwords and
// session cookies never cross the network in clear text. It uses only Node
// built-ins and forwards to Vite, which proxies /api to the Agent.
import fs from 'node:fs';
import http from 'node:http';
import https from 'node:https';
import net from 'node:net';
import { createServer } from 'vite';

const UI_HOST = process.env.ARKNET_UI_HOST || '127.0.0.1';
const UI_PORT = 5173;
const HTTPS_PORT = Number(process.env.ARKNET_UI_HTTPS_PORT || 8443);
const CERT = process.env.ARKNET_UI_TLS_CERT || '/etc/arknet/tls/cert.pem';
const KEY = process.env.ARKNET_UI_TLS_KEY || '/etc/arknet/tls/key.pem';
// Vite rejects unknown Host headers; the HTTPS listener talks to it as loopback.
const UPSTREAM_HOST = `127.0.0.1:${UI_PORT}`;

export const SECURITY_HEADERS = {
  'x-content-type-options': 'nosniff',
  'referrer-policy': 'no-referrer',
  'x-frame-options': 'DENY',
  // No HSTS: a self-signed host must not pin itself in browsers.
};

export function forwardHeaders(headers, socket) {
  const out = { ...headers, host: UPSTREAM_HOST, 'x-forwarded-proto': 'https' };
  if (headers.host) out['x-forwarded-host'] = headers.host;
  out['x-forwarded-for'] = socket.remoteAddress ?? '';
  return out;
}

export function startHttps({ cert, key, port = HTTPS_PORT, upstreamPort = UI_PORT, host = '0.0.0.0' }) {
  const server = https.createServer({ cert, key, minVersion: 'TLSv1.2' }, (req, res) => {
    const upstream = http.request({ host: '127.0.0.1', port: upstreamPort, method: req.method, path: req.url,
      headers: forwardHeaders(req.headers, req.socket) }, answer => {
      res.writeHead(answer.statusCode ?? 502, { ...answer.headers, ...SECURITY_HEADERS });
      answer.pipe(res);
    });
    upstream.on('error', () => { if (!res.headersSent) res.writeHead(502, SECURITY_HEADERS); res.end(); });
    req.pipe(upstream);
  });
  // Vite's hot-reload WebSocket.
  server.on('upgrade', (req, socket, head) => {
    const upstream = net.connect(upstreamPort, '127.0.0.1', () => {
      const lines = Object.entries(forwardHeaders(req.headers, req.socket)).map(([k, v]) => `${k}: ${v}`);
      upstream.write(`${req.method} ${req.url} HTTP/1.1\r\n${lines.join('\r\n')}\r\n\r\n`);
      upstream.write(head);
      socket.pipe(upstream).pipe(socket);
    });
    upstream.on('error', () => socket.destroy());
    socket.on('error', () => upstream.destroy());
  });
  return new Promise(resolve => server.listen(port, host, () => resolve(server)));
}

export function readTLS(certFile = CERT, keyFile = KEY) {
  try {
    return { cert: fs.readFileSync(certFile), key: fs.readFileSync(keyFile) };
  } catch {
    return null;
  }
}

export function dropPrivileges(runtime = process, uid = Number(process.env.ARKNET_UI_UID || 1000), gid = Number(process.env.ARKNET_UI_GID || 1000)) {
  if (runtime.getuid?.() !== 0) return;
  runtime.setgroups([]);
  runtime.setgid(gid);
  runtime.setuid(uid);
}

if (import.meta.url === `file://${process.argv[1]}`) {
  // Read the root-owned mode-0600 key before permanently dropping privileges.
  // The key remains only in process memory; listeners and request handling run
  // as the unprivileged node user.
  const tls = readTLS();
  dropPrivileges();
  const vite = await createServer({ configLoader: 'runner', cacheDir: '/tmp/arknet-vite-cache',
    server: { host: UI_HOST, port: UI_PORT, strictPort: true } });
  await vite.listen();
  vite.printUrls();
  if (tls) {
    await startHttps(tls);
    console.log(`  ➜  HTTPS:   https://0.0.0.0:${HTTPS_PORT}/ (self-signed Node certificate)`);
  } else {
    console.warn(`  !  No TLS certificate at ${CERT}; the HTTPS listener is off. Run \`sudo arknet-node upgrade\` or \`arknet-node tls renew\`.`);
  }
}
