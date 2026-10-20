import { defineConfig } from '@playwright/test';

export default defineConfig({
  testDir: './e2e',
  testMatch: 'chat.spec.ts',
  forbidOnly: !!process.env.CI,
  workers: 1,
  reporter: 'list',
  use: { baseURL: 'http://localhost:4322/waza' },
  projects: [{ name: 'chromium', use: { browserName: 'chromium' } }],
  webServer: {
    command: 'npm run build && npm run preview -- --port 4322',
    port: 4322,
    reuseExistingServer: false,
    timeout: 120000,
    env: {
      PUBLIC_CHAT_ENABLED: 'true',
      PUBLIC_CHAT_API_URL: 'https://waza-chat.example/api',
      PUBLIC_CHAT_TURNSTILE_SITEKEY: 'test-public-sitekey',
    },
  },
});
