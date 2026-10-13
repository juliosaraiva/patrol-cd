/// <reference types="vitest/config" />
import { readFile, writeFile } from 'node:fs/promises';
import { createRequire } from 'node:module';
import path from 'node:path';
import react from '@vitejs/plugin-react';
import { defineConfig, loadEnv, type Plugin } from 'vite';

const require = createRequire(import.meta.url);

/**
 * The Go server embeds ui/dist, and git tracks ui/dist/.keep so the embed pattern always
 * matches. Vite empties the output directory on every build, so restore the placeholder.
 */
function keepDistPlaceholder(): Plugin {
  let outDir = 'dist';
  return {
    name: 'patrol:keep-dist-placeholder',
    apply: 'build',
    configResolved(config) {
      outDir = path.resolve(config.root, config.build.outDir);
    },
    async closeBundle() {
      await writeFile(path.join(outDir, '.keep'), '');
    },
  };
}

/**
 * Dev mock mode (VITE_MOCK_API=1) needs MSW's service worker script at the site root. Serve it
 * straight from the msw package during `vite dev` so it is never copied into production output.
 */
function mockServiceWorker(enabled: boolean): Plugin {
  return {
    name: 'patrol:msw-worker',
    apply: 'serve',
    configureServer(server) {
      if (!enabled) return;
      const workerPath = require.resolve('msw/mockServiceWorker.js');
      server.middlewares.use('/mockServiceWorker.js', (_req, res) => {
        void readFile(workerPath).then((body) => {
          res.setHeader('Content-Type', 'text/javascript');
          res.setHeader('Service-Worker-Allowed', '/');
          res.end(body);
        });
      });
    },
  };
}

export default defineConfig(({ mode }) => {
  const env = loadEnv(mode, process.cwd(), '');
  const mockApi = env.VITE_MOCK_API === '1';
  // PATROL_API_URL points `vite dev` at a running patrol-server (e.g. https://localhost:8080).
  const apiTarget = env.PATROL_API_URL;
  const proxy = apiTarget
    ? Object.fromEntries(
        ['/api', '/auth', '/extensions', '/terminal'].map((prefix) => [
          prefix,
          { target: apiTarget, changeOrigin: true, secure: false, ws: prefix === '/terminal' },
        ]),
      )
    : undefined;

  return {
    // Relative asset URLs: the server injects <base href="<rootpath>/"> and every asset resolves
    // against it, so one build serves any --rootpath / --basehref.
    base: './',
    plugins: [react(), keepDistPlaceholder(), mockServiceWorker(mockApi)],
    build: {
      outDir: 'dist',
      emptyOutDir: true,
      target: 'es2022',
      assetsDir: 'assets',
    },
    server: {
      port: 5173,
      strictPort: false,
      ...(proxy && !mockApi ? { proxy } : {}),
    },
    test: {
      environment: 'jsdom',
      include: ['src/**/*.test.{ts,tsx}', 'scripts/**/*.test.mjs'],
      setupFiles: ['./src/test/setup.ts'],
      css: { modules: { classNameStrategy: 'non-scoped' } },
      restoreMocks: true,
      testTimeout: 10000,
    },
  };
});
