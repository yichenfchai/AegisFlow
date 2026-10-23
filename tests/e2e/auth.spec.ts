import { test, expect } from '@playwright/test'
import type { Page } from '@playwright/test'

const admin = { username: 'e2e-admin', password: 'E2e-admin-password-2026' }
async function login(page: Page, remember = true) {
  await page.goto('/login')
  await page.getByLabel('用户名', { exact: true }).fill(admin.username)
  await page.getByLabel('密码', { exact: true }).fill(admin.password)
  const checkbox = page.getByRole('checkbox')
  await checkbox.setChecked(remember)
  await page.getByRole('button', { name: '登录', exact: true }).click()
  await expect(page).toHaveURL('/')
}

test('真实登录、恢复、并发刷新、多标签页和退出撤销', async ({ page, context }) => {
  await page.goto('/settings')
  await expect(page).toHaveURL(/\/login/)
  await page.getByLabel('用户名', { exact: true }).fill(admin.username)
  await page.getByLabel('密码', { exact: true }).fill('incorrect-password')
  await page.getByRole('button', { name: '登录', exact: true }).click()
  await expect(page.getByText(/账号、密码错误/)).toBeVisible()
  await login(page)
  const cookies = await context.cookies()
  const refresh = cookies.find(c => c.name === 'aegis_refresh')!
  expect(refresh.httpOnly).toBe(true)
  expect(refresh.sameSite).toBe('Strict')
  expect(refresh.expires).toBeGreaterThan(Date.now() / 1000)
  expect(await page.evaluate(() => localStorage.getItem('gp_session'))).toBeNull()
  expect(await page.evaluate(() => document.cookie)).not.toContain('aegis_refresh')
  await page.reload()
  await expect(page.getByRole('button', { name: '退出登录' })).toBeVisible()
  const other = await context.newPage()
  await Promise.all([other.goto('/'), page.reload()])
  await expect(other.getByRole('button', { name: '退出登录' })).toBeVisible()
  // Allow the three-second access token to expire while leaving the real refresh cookie valid.
  await page.waitForTimeout(3200)
  const result = await page.evaluate(async () => {
    const { apiRequest, useSession } = await import('/src/stores/session.ts')
    const values = await Promise.all(Array.from({ length: 5 }, () => apiRequest<{ username: string }>('/auth/me')))
    return { values, token: useSession.getState().accessToken }
  })
  expect(result.values.every(u => u.username === admin.username)).toBe(true)
  await page.getByRole('button', { name: '退出登录' }).click()
  await expect(page).toHaveURL(/\/login/)
  await expect(other).toHaveURL(/\/login/)
  const revoked = await page.request.get('/api/v1/auth/me', { headers: { Authorization: `Bearer ${result.token}` } })
  expect(revoked.status()).toBe(401)
  await page.reload()
  await expect(page).toHaveURL(/\/login/)
})

test('401最多重试一次、403不刷新、退出失败可重试', async ({ page }) => {
  await login(page)
  let attempts = 0
  await page.route('**/api/v1/auth/me', async route => {
    attempts++
    if (attempts === 1) await route.fulfill({ status: 401, contentType: 'application/json', body: JSON.stringify({ error: { code: 'unauthorized', message: 'expired' } }) })
    else await route.continue()
  })
  const retried = await page.evaluate(async () => {
    const { apiRequest } = await import('/src/stores/session.ts')
    return apiRequest<{ username: string }>('/auth/me')
  })
  expect(retried.username).toBe(admin.username)
  expect(attempts).toBe(2)
  await page.unroute('**/api/v1/auth/me')
  await page.route('**/api/v1/auth/logout', route => route.abort('failed'))
  await page.getByRole('button', { name: '退出登录' }).click()
  await expect(page.getByText(/无法连接服务/)).toBeVisible()
  await expect(page).toHaveURL('/')
  await page.unroute('**/api/v1/auth/logout')
  await page.getByRole('button', { name: '退出登录' }).click()
  await expect(page).toHaveURL(/\/login/)
})

test('会话 Cookie、失效刷新及非管理员限制', async ({ page, context }) => {
  await login(page, false)
  const cookie = (await context.cookies()).find(c => c.name === 'aegis_refresh')!
  expect(cookie.expires).toBe(-1)
  const username = `viewer-${Date.now()}`
  const password = 'Viewer-password-2026'
  const created = await page.evaluate(async ({ username, password }) => {
    const { apiRequest } = await import('/src/stores/session.ts')
    return apiRequest<{ role: string }>('/users', { method: 'POST', body: JSON.stringify({ username, password, displayName: 'Test Viewer', role: 'viewer' }) })
  }, { username, password })
  expect(created.role).toBe('viewer')
  await page.getByRole('button', { name: '退出登录' }).click()
  await page.getByLabel('用户名', { exact: true }).fill(username)
  await page.getByLabel('密码', { exact: true }).fill(password)
  await page.getByRole('button', { name: '登录', exact: true }).click()
  await expect(page).toHaveURL('/')
  const status = await page.evaluate(async () => {
    const { apiRequest } = await import('/src/stores/session.ts')
    try { await apiRequest('/users'); return 200 } catch (error) { return (error as { status: number }).status }
  })
  expect(status).toBe(403)
  await page.goto('/settings')
  await expect(page.getByText(/无权访问|权限不足|没有权限/).first()).toBeVisible()
  await context.clearCookies()
  await page.reload()
  await expect(page).toHaveURL(/\/login/)
})
