// ArkNet Node management UI production server (Prompts 45B, 51).
//
// Serves the prebuilt static UI (`dist/`, from `npm run build`) and proxies
// `/api` to the Agent, using only Node built-ins: the runtime image ships no
// Vite, no dependencies and no source maps. Plain HTTP listens on 5173
// (published on the host's loopback only, for SSH tunnels). When the Node's
// self-signed certificate is present, an HTTPS listener on 8443 serves the same
// UI to the LAN, so operator passwords and session cookies never cross the
// network in clear text. No request URL, header or body is ever logged: grants
// can appear in URL paths and queries.
import fs from 'node:fs';
import http from 'node:http';
import https from 'node:https';
import path from 'node:path';

const UI_HOST = process.env.ARKNET_UI_HOST || '127.0.0.1';
const UI_PORT = 5173;
const HTTPS_PORT = Number(process.env.ARKNET_UI_HTTPS_PORT || 8443);
const CERT = process.env.ARKNET_UI_TLS_CERT || '/etc/arknet/tls/cert.pem';
const KEY = process.env.ARKNET_UI_TLS_KEY || '/etc/arknet/tls/key.pem';
// The Agent has always seen HTTPS-listener requests as coming from the
// loopback UI; keep that contract now that the hop is in-process.
const UPSTREAM_HOST = `127.0.0.1:${UI_PORT}`;
export const PROXY_ERROR = 'http proxy error: upstream request failed (details redacted)';

export const SECURITY_HEADERS = {
  'x-content-type-options': 'nosniff',
  'referrer-policy': 'no-referrer',
  'x-frame-options': 'DENY',
  // No HSTS: a self-signed host must not pin itself in browsers.
};

// Only the UI's own documents get a CSP; proxied API responses pass through.
const STATIC_HEADERS = {
  ...SECURITY_HEADERS,
  'content-security-policy': "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
    "img-src 'self' data:; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'",
};

const TYPES = {
  '.html': 'text/html; charset=utf-8', '.js': 'text/javascript; charset=utf-8', '.css': 'text/css; charset=utf-8',
  '.svg': 'image/svg+xml', '.png': 'image/png', '.ico': 'image/x-icon', '.json': 'application/json',
  '.woff2': 'font/woff2', '.txt': 'text/plain; charset=utf-8', '.webmanifest': 'application/manifest+json',
};

// Same routing as the Vite development proxy (vite.config.ts): public and
// member routes live on the member listener; everything else under /api,
// including operator session reads/logout, goes to the management listener.
const MEMBER_PREFIXES = ['/api/v1/operator/setup', '/api/v1/operator/sessions', '/api/v1/local-users/sessions',
  '/api/v1/global-users/sessions', '/api/v1/global-users/login'];
const MEMBER_EXACT = /^\/api\/v1\/(local|global)-users\/session$/;

export function upstreamFor(pathname) {
  if (MEMBER_PREFIXES.some(prefix => pathname.startsWith(prefix)) || MEMBER_EXACT.test(pathname)) return 'member';
  if (pathname === '/api' || pathname.startsWith('/api/')) return 'agent';
  return null;
}

const HOP_BY_HOP = new Set(['connection', 'keep-alive', 'proxy-connection', 'proxy-authenticate', 'proxy-authorization',
  'te', 'trailer', 'transfer-encoding', 'upgrade']);

function withoutHopByHop(headers) {
  const out = {};
  for (const [name, value] of Object.entries(headers)) if (!HOP_BY_HOP.has(name.toLowerCase())) out[name] = value;
  return out;
}

export function forwardHeaders(headers, socket) {
  const out = { ...headers, host: UPSTREAM_HOST, 'x-forwarded-proto': 'https' };
  if (headers.host) out['x-forwarded-host'] = headers.host;
  out['x-forwarded-for'] = socket.remoteAddress ?? '';
  return out;
}

// Load the whole build into memory at startup: lookups are exact-path map hits,
// so no request can name a file outside the build.
export function loadStatic(directory) {
  const files = new Map();
  const walk = dir => {
    for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
      const full = path.join(dir, entry.name);
      if (entry.isDirectory()) walk(full);
      else if (entry.isFile() && !entry.name.endsWith('.map')) {
        const route = '/' + path.relative(directory, full).split(path.sep).join('/');
        files.set(route, { body: fs.readFileSync(full), type: TYPES[path.extname(entry.name)] || 'application/octet-stream' });
      }
    }
  };
  walk(directory);
  if (!files.has('/index.html')) throw new Error(`No index.html in ${directory}; run \`npm run build\` first`);
  return files;
}

function serveStatic(files, req, res, pathname) {
  if (req.method !== 'GET' && req.method !== 'HEAD') {
    res.writeHead(405, { ...STATIC_HEADERS, allow: 'GET, HEAD' });
    return res.end();
  }
  let route;
  try { route = decodeURIComponent(pathname); } catch { route = null; }
  let file = route === null ? undefined : files.get(route === '/' ? '/index.html' : route);
  let cache = route?.startsWith('/assets/') ? 'public, max-age=31536000, immutable' : 'no-cache';
  // Client-side routes (for example /member, /login/callback) get the app shell;
  // a missing asset or file-like path is a real 404.
  if (!file && route !== null && !route.startsWith('/assets/') && !path.posix.extname(route)) {
    file = files.get('/index.html');
    cache = 'no-cache';
  }
  if (!file) {
    res.writeHead(404, { ...STATIC_HEADERS, 'content-type': 'text/plain; charset=utf-8' });
    return res.end('Not found\n');
  }
  res.writeHead(200, { ...STATIC_HEADERS, 'content-type': file.type, 'content-length': file.body.length, 'cache-control': cache });
  res.end(req.method === 'HEAD' ? undefined : file.body);
}

function proxy(req, res, target, headers, log) {
  const upstream = http.request({ hostname: target.hostname, port: target.port || 80, method: req.method,
    path: req.url, headers: withoutHopByHop(headers) }, answer => {
    res.writeHead(answer.statusCode ?? 502, { ...withoutHopByHop(answer.headers), ...SECURITY_HEADERS });
    answer.pipe(res);
  });
  upstream.on('error', () => {
    log(PROXY_ERROR);
    if (!res.headersSent) res.writeHead(502, SECURITY_HEADERS);
    res.end();
  });
  res.on('close', () => { if (!res.writableFinished) upstream.destroy(); });
  req.pipe(upstream);
}

export function createHandler({ staticDir, agentUrl, memberUrl, log = message => console.error(message) }) {
  const files = loadStatic(staticDir);
  const targets = { agent: new URL(agentUrl), member: new URL(memberUrl) };
  return (req, res, viaHttps = false) => {
    let pathname = null;
    if (req.url.startsWith('/')) {
      try { pathname = new URL(req.url, 'http://ui.invalid').pathname; } catch { /* Malformed target. */ }
    }
    if (pathname === null) {
      res.writeHead(400, SECURITY_HEADERS);
      return res.end();
    }
    const upstream = upstreamFor(pathname);
    if (upstream) return proxy(req, res, targets[upstream], viaHttps ? forwardHeaders(req.headers, req.socket) : req.headers, log);
    return serveStatic(files, req, res, pathname);
  };
}

const quietErrors = server => server.on('clientError', (_error, socket) => socket.destroy());

export function startHttp({ handler, port = UI_PORT, host = UI_HOST }) {
  const server = http.createServer((req, res) => handler(req, res, false));
  quietErrors(server);
  return new Promise(resolve => server.listen(port, host, () => resolve(server)));
}

export function startHttps({ cert, key, handler, port = HTTPS_PORT, host = '0.0.0.0' }) {
  const server = https.createServer({ cert, key, minVersion: 'TLSv1.2' }, (req, res) => handler(req, res, true));
  quietErrors(server);
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
  const handler = createHandler({
    staticDir: process.env.ARKNET_UI_STATIC_DIR || path.join(import.meta.dirname, 'dist'),
    agentUrl: process.env.ARKNET_AGENT_URL || 'http://127.0.0.1:8081',
    memberUrl: process.env.ARKNET_MEMBER_URL || 'http://127.0.0.1:8082',
  });
  const servers = [await startHttp({ handler })];
  console.log(`  ➜  Local:   http://${UI_HOST}:${UI_PORT}/`);
  if (tls) {
    servers.push(await startHttps({ ...tls, handler }));
    console.log(`  ➜  HTTPS:   https://0.0.0.0:${HTTPS_PORT}/ (self-signed Node certificate)`);
  } else {
    console.warn(`  !  No TLS certificate at ${CERT}; the HTTPS listener is off. Run \`sudo arknet-node upgrade\` or \`arknet-node tls renew\`.`);
  }
  // As PID 1 in a container, Node has no default SIGTERM handling.
  for (const signal of ['SIGTERM', 'SIGINT']) {
    process.once(signal, () => {
      for (const server of servers) { server.close(); server.closeAllConnections?.(); }
      setTimeout(() => process.exit(0), 2000).unref();
    });
  }
}
