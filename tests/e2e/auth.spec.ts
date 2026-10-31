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

test('账号管理：创建、重置、禁用、审计与自助安全', async ({ page }) => {
  await login(page)
  // 管理员通过真实 UI 创建成员（唯一用户名，避免数据库残留导致冲突）。
  const memberName = `acct-${Date.now()}`
  await page.goto('/settings')
  await page.getByRole('button', { name: '创建用户' }).click()
  await page.getByLabel('用户名').fill(memberName)
  await page.getByLabel('显示名').fill('账号测试')
  await page.getByLabel('初始密码').fill('Acct-password-2026')
  await page.getByLabel('角色').selectOption('member')
  await page.getByRole('button', { name: '创建', exact: true }).click()
  const row = page.getByRole('row', { name: new RegExp(memberName) })
  await expect(row).toBeVisible()
  // 变更角色。
  await row.getByRole('combobox').selectOption('viewer')
  await expect(row.getByRole('combobox')).toHaveValue('viewer')
  // 重置密码并禁用。
  await row.getByRole('button', { name: '重置密码' }).click()
  await page.getByLabel('新密码').fill('Acct-newpass-2026')
  await page.getByRole('button', { name: '确认重置' }).click()
  await expect(row.getByRole('button', { name: '禁用', exact: true })).toBeVisible()
  await row.getByRole('button', { name: '禁用', exact: true }).click()
  await page.getByRole('button', { name: '确认禁用' }).click()
  await expect(row.getByText('已禁用')).toBeVisible()
  // 审计页可查询全部管理操作。断言限定在表格内：筛选下拉里也有同名选项。
  await page.getByRole('button', { name: '审计', exact: true }).click()
  const auditTable = page.getByRole('table')
  for (const action of ['创建用户', '变更角色', '重置密码', '禁用账号']) {
    await expect(auditTable.getByText(action).first()).toBeVisible()
  }
  // 禁用账号即使密码正确也无法登录（403 account_disabled）。
  const denied = await page.request.post('/api/v1/auth/login', {
    data: { username: memberName, password: 'Acct-newpass-2026' },
    headers: { Origin: 'http://localhost:5173' },
  })
  expect(denied.status()).toBe(403)
  // 重新启用后成员可登录，但无权访问管理页。
  await page.getByRole('button', { name: '用户管理' }).click()
  await row.getByRole('button', { name: '启用', exact: true }).click()
  await page.getByRole('button', { name: '确认启用' }).click()
  await expect(row.getByText('正常', { exact: true })).toBeVisible()

  const browser = page.context().browser()!
  const memberCtx = await browser.newContext()
  const member = await memberCtx.newPage()
  await member.goto('/login')
  await member.getByLabel('用户名', { exact: true }).fill(memberName)
  await member.getByLabel('密码', { exact: true }).fill('Acct-newpass-2026')
  await member.getByRole('button', { name: '登录', exact: true }).click()
  await expect(member).toHaveURL('/')
  await member.goto('/settings')
  await expect(member.getByText('无权访问')).toBeVisible()
  // 自助修改密码：保留当前会话。
  await member.goto('/account')
  await member.getByLabel('当前密码').fill('Acct-newpass-2026')
  await member.getByLabel('新密码', { exact: true }).fill('Acct-final-2026')
  await member.getByLabel('确认新密码').fill('Acct-final-2026')
  await member.getByRole('button', { name: '更新密码' }).click()
  await expect(member.getByText('密码已更新')).toBeVisible()
  // 第二个成员会话登录后，撤销其他全部会话立即踢出。
  const ctx2 = await browser.newContext()
  const member2 = await ctx2.newPage()
  await member2.goto('/login')
  await member2.getByLabel('用户名', { exact: true }).fill(memberName)
  await member2.getByLabel('密码', { exact: true }).fill('Acct-final-2026')
  await member2.getByRole('button', { name: '登录', exact: true }).click()
  await expect(member2).toHaveURL('/')
  await member.reload()
  await expect(member.locator('tbody tr')).toHaveCount(2)
  await member.getByRole('button', { name: '撤销其他全部会话' }).click()
  await member.getByRole('button', { name: '确认撤销' }).click()
  await expect(member.locator('tbody tr')).toHaveCount(1)
  await member2.reload()
  await expect(member2).toHaveURL(/\/login/)
  await memberCtx.close()
  await ctx2.close()
})

test('项目管理：创建、归档恢复与角色限制', async ({ page }) => {
  await login(page)
  // 通过真实 UI 创建项目（唯一名称，避免跨运行残留）。
  await page.goto('/')
  await page.getByRole('link', { name: '创建项目' }).click()
  const name = `e2e-proj-${Date.now()}`
  await page.getByLabel('项目名称').fill(name)
  await page.getByLabel('描述').fill('E2E 验证项目')
  await page.getByLabel('仓库地址 / 本地路径').fill('https://github.com/example/e2e.git')
  await page.getByLabel('默认分支').selectOption('main')
  await page.getByRole('button', { name: '创建项目', exact: true }).click()
  await expect(page).toHaveURL('/')
  const row = page.getByRole('row', { name: new RegExp(name) })
  await expect(row.getByText('活跃')).toBeVisible()
  // 归档后只读，恢复后回到活跃。
  await row.getByRole('button', { name: '归档', exact: true }).click()
  await page.getByRole('button', { name: '确认归档' }).click()
  await expect(row.getByText('已归档')).toBeVisible()
  await row.getByRole('button', { name: '恢复', exact: true }).click()
  await page.getByRole('button', { name: '确认恢复' }).click()
  await expect(row.getByText('活跃')).toBeVisible()
  // 管理操作进入审计。
  await page.goto('/settings')
  await page.getByRole('button', { name: '审计', exact: true }).click()
  const projectAuditTable = page.getByRole('table')
  await expect(projectAuditTable.getByText('创建项目').first()).toBeVisible()
  await expect(projectAuditTable.getByText('归档项目').first()).toBeVisible()
  await expect(projectAuditTable.getByText('恢复项目').first()).toBeVisible()
  // viewer 可读列表但没有创建入口，直连 API 也被拒绝。
  const username = `proj-viewer-${Date.now()}`
  await page.getByRole('button', { name: '用户管理' }).click()
  await page.evaluate(async ({ username }) => {
    const { apiRequest } = await import('/src/stores/session.ts')
    return apiRequest('/users', { method: 'POST', body: JSON.stringify({ username, password: 'Viewer-pass-2026', displayName: 'Proj Viewer', role: 'viewer' }) })
  }, { username })
  const browser = page.context().browser()!
  const viewerCtx = await browser.newContext()
  const viewer = await viewerCtx.newPage()
  await viewer.goto('/login')
  await viewer.getByLabel('用户名', { exact: true }).fill(username)
  await viewer.getByLabel('密码', { exact: true }).fill('Viewer-pass-2026')
  await viewer.getByRole('button', { name: '登录', exact: true }).click()
  await expect(viewer).toHaveURL('/')
  await expect(viewer.getByRole('link', { name: '创建项目' })).toHaveCount(0)
  await expect(viewer.getByText(name)).toBeVisible()
  const status = await viewer.evaluate(async () => {
    const { apiRequest } = await import('/src/stores/session.ts')
    try {
      await apiRequest('/projects', { method: 'POST', body: JSON.stringify({ name: 'viewer-denied-x' }) })
      return 201
    } catch (error) {
      return (error as { status: number }).status
    }
  })
  expect(status).toBe(403)
  await viewerCtx.close()
})
