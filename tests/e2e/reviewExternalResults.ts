import { createHmac } from 'node:crypto'

/**
 * 固定 E2E 外部结果 Webhook 密钥：`playwright.config.ts` 把它注入后端 webServer
 * 的 `AEGIS_REVIEW_WEBHOOK_SECRET`，用例用同一值计算 HMAC。它只存在于本地一次性
 * E2E 运行中，不是生产凭据，也不应复用到任何真实部署。
 */
export const E2E_REVIEW_WEBHOOK_SECRET = 'aegisflow-e2e-review-webhook-secret-2026'

/**
 * 后端契约：`sha256=<hex HMAC-SHA256(secret, timestamp + "." + rawBody)>`，对原始
 * 请求体字节签名（不是对重新编码后的 JSON 签名）。
 */
export function signExternalResult(secret: string, timestamp: string, rawBody: string): string {
  return `sha256=${createHmac('sha256', secret).update(`${timestamp}.${rawBody}`).digest('hex')}`
}
