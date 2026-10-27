import { defineConfig } from '@playwright/test'
import { randomBytes } from 'node:crypto'

if (!process.env.AEGIS_E2E_DATABASE_URL || !process.env.AEGIS_E2E_REDIS_URL) {
  throw new Error('Set AEGIS_E2E_DATABASE_URL to a dedicated disposable PostgreSQL database and AEGIS_E2E_REDIS_URL before E2E tests')
}

export default defineConfig({
  testDir: './tests/e2e',
  fullyParallel: false,
  workers: 1,
  timeout: 45_000,
  use: { baseURL: 'http://localhost:5173', browserName: 'chromium', trace: 'retain-on-failure' },
  webServer: [
    {
      command: 'go run ./cmd/server',
      cwd: './backend',
      url: 'http://127.0.0.1:8080/readyz',
      reuseExistingServer: false,
      timeout: 120_000,
      env: {
        AEGIS_HTTP_ADDR: '127.0.0.1:8080',
        AEGIS_DATABASE_PATH: '',
        AEGIS_DATABASE_URL: process.env.AEGIS_E2E_DATABASE_URL!,
        AEGIS_REDIS_URL: process.env.AEGIS_E2E_REDIS_URL!,
        AEGIS_REDIS_NAMESPACE: `e2e-${randomBytes(8).toString('hex')}`,
        AEGIS_JWT_SECRET: randomBytes(48).toString('base64'),
        AEGIS_BOOTSTRAP_USERNAME: 'e2e-admin',
        AEGIS_BOOTSTRAP_PASSWORD: 'E2e-admin-password-2026',
        AEGIS_ACCESS_TTL: '3s',
        AEGIS_REFRESH_TTL: '1h',
        AEGIS_COOKIE_SECURE: 'false',
        AEGIS_ALLOWED_ORIGINS: 'http://localhost:5173',
        // The suite drives many accounts from one address inside a minute; the
        // production default quota of 10 would reject valid logins mid-run.
        AEGIS_LOGIN_RATE_LIMIT: '100',
      },
    },
    { command: 'npm run dev -- --host localhost --strictPort', url: 'http://localhost:5173', reuseExistingServer: false },
  ],
})
