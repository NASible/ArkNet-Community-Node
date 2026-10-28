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
        '/api': { target: env.ARKNET_AGENT_URL || 'http://127.0.0.1:8081' },
      },
    },
    test: { environment: 'jsdom', restoreMocks: true },
  };
});
