import { defineConfig } from '@playwright/test'
import { randomBytes } from 'node:crypto'
import { E2E_REVIEW_WEBHOOK_SECRET } from './tests/e2e/reviewExternalResults'
import { E2E_INGEST_WEBHOOK_SECRET } from './tests/e2e/ingestWebhook'

if (!process.env.AEGIS_E2E_DATABASE_URL || !process.env.AEGIS_E2E_REDIS_URL) {
  throw new Error('Set AEGIS_E2E_DATABASE_URL to a dedicated disposable PostgreSQL database and AEGIS_E2E_REDIS_URL before E2E tests')
}

// Optional OIDC SSO suite (P5 batch D). AEGIS_E2E_OIDC=1 starts the local fake
// provider and injects AEGIS_OIDC_* into the backend, so the login page shows
// the SSO button and tests/e2e/oidc.spec.ts runs. Without it the default suite
// is unchanged and proves the unconfigured deployment has no SSO button.
const oidcE2E = process.env.AEGIS_E2E_OIDC === '1'
const oidcProviderPort = 9099
const oidcProviderBase = `http://127.0.0.1:${oidcProviderPort}`

export default defineConfig({
  testDir: './tests/e2e',
  // The SSO spec needs the fake provider and OIDC-enabled backend, so it is
  // excluded from the default (unconfigured) run rather than silently skipped.
  testIgnore: oidcE2E ? [] : ['**/oidc.spec.ts'],
  fullyParallel: false,
  workers: 1,
  timeout: 45_000,
  use: { baseURL: 'http://localhost:5173', browserName: 'chromium', trace: 'retain-on-failure' },
  webServer: [
    ...(oidcE2E
      ? [
          {
            command: 'node tests/e2e/fakeOidcProvider.mjs',
            url: `${oidcProviderBase}/.well-known/openid-configuration`,
            reuseExistingServer: false,
            timeout: 60_000,
            env: { AEGIS_FAKE_OIDC_PORT: String(oidcProviderPort) },
          },
        ]
      : []),
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
        // Deployment-target credentials and the independent secret store
        // (/keys, write-only AES-256-GCM values) are sealed with this key
        // (base64 of exactly 32 bytes); a per-run random key keeps stored
        // secrets readable for this run only, matching the disposable E2E
        // database. The secrets E2E spec needs it for create/rotate.
        AEGIS_SECRET_KEY: randomBytes(32).toString('base64'),
        AEGIS_BOOTSTRAP_USERNAME: 'e2e-admin',
        AEGIS_BOOTSTRAP_PASSWORD: 'E2e-admin-password-2026',
        AEGIS_ACCESS_TTL: '3s',
        AEGIS_REFRESH_TTL: '1h',
        AEGIS_COOKIE_SECURE: 'false',
        AEGIS_ALLOWED_ORIGINS: 'http://localhost:5173',
        // The suite drives many accounts from one address inside a minute; the
        // production default quota of 10 would reject valid logins mid-run.
        AEGIS_LOGIN_RATE_LIMIT: '100',
        // Fixed test-only HMAC secret so the external-results spec can sign
        // deliveries against the same value the backend verifies. It never
        // leaves the disposable E2E environment.
        AEGIS_REVIEW_WEBHOOK_SECRET: E2E_REVIEW_WEBHOOK_SECRET,
        // Fixed test-only HMAC secret for the signed scan/security-event ingest
        // webhooks (scans-defense spec); also disposable and never reused.
        AEGIS_INGEST_WEBHOOK_SECRET: E2E_INGEST_WEBHOOK_SECRET,
        // Optional fake-provider SSO (see AEGIS_E2E_OIDC above). The redirect
        // URL is the browser-visible Vite origin, which proxies /api to the
        // backend; the issuer is the local throwaway provider.
        ...(oidcE2E
          ? {
              AEGIS_OIDC_ISSUER: oidcProviderBase,
              AEGIS_OIDC_CLIENT_ID: 'aegisflow-e2e',
              AEGIS_OIDC_REDIRECT_URL: 'http://localhost:5173/api/v1/auth/oidc/callback',
              AEGIS_OIDC_AUTO_PROVISION: 'true',
              AEGIS_OIDC_LABEL: '企业单点登录',
            }
          : {}),
      },
    },
    { command: 'npm run dev -- --host localhost --strictPort', url: 'http://localhost:5173', reuseExistingServer: false },
  ],
})
