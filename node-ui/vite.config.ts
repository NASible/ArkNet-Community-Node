import { stripVTControlCharacters } from 'node:util';
import { loadEnv } from 'vite';
import { defineConfig } from 'vitest/config';

export default defineConfig(({ mode }) => {
  const env = loadEnv(mode, process.cwd(), '');
  return {
    plugins: [{
      name: 'arknet-private-proxy-diagnostics',
      configResolved(config) {
        // Vite includes req.url and err.stack/error in proxy failures. Grants
        // live in URL paths as well as queries, so do not attempt token parsing
        // or mutate the request: replace only these diagnostic records in full.
        // Wrap the resolved logger so custom log sinks get the same protection.
        const error = config.logger.error.bind(config.logger);
        config.logger.error = (message, options) => {
          const kind = /^(http proxy error|ws proxy error|ws proxy socket error):/.exec(stripVTControlCharacters(message))?.[1];
          if (kind) {
            error(`${kind}: upstream request failed (details redacted)`, { timestamp: true });
          } else {
            // Keep normal build/configuration diagnostics and their metadata.
            error(message, options);
          }
        };
      },
    }],
    server: {
      host: '127.0.0.1',
      port: 5173,
      strictPort: true,
      proxy: {
        // Public/member routes live on the member listener. Operator session
        // reads/logout and every management API stay on the management listener.
        '/api/v1/operator/setup': { target: env.ARKNET_MEMBER_URL || 'http://127.0.0.1:8082' },
        '/api/v1/operator/sessions': { target: env.ARKNET_MEMBER_URL || 'http://127.0.0.1:8082' },
        '/api/v1/local-users/sessions': { target: env.ARKNET_MEMBER_URL || 'http://127.0.0.1:8082' },
        '/api/v1/global-users/sessions': { target: env.ARKNET_MEMBER_URL || 'http://127.0.0.1:8082' },
        // Core-bound member login (start and callback completion).
        '/api/v1/global-users/login': { target: env.ARKNET_MEMBER_URL || 'http://127.0.0.1:8082' },
        // Exact match: single-session member reads/sign-out.
        '^/api/v1/(local|global)-users/session$': { target: env.ARKNET_MEMBER_URL || 'http://127.0.0.1:8082' },
        '/api': { target: env.ARKNET_AGENT_URL || 'http://127.0.0.1:8081' },
      },
    },
    // The production image must never ship source maps (Prompt 51).
    build: { sourcemap: false },
    test: { environment: 'jsdom', restoreMocks: true },
  };
});
