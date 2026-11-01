import { loadEnv } from 'vite';
import { defineConfig } from 'vitest/config';

export default defineConfig(({ mode }) => {
  const env = loadEnv(mode, process.cwd(), '');
  return {
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
    test: { environment: 'jsdom', restoreMocks: true },
  };
});
