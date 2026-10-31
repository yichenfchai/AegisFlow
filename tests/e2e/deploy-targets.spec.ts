import { test, expect } from '@playwright/test'
import type { Page } from '@playwright/test'

const admin = { username: 'e2e-admin', password: 'E2e-admin-password-2026' }

async function login(page: Page, username = admin.username, password = admin.password) {
  await page.goto('/login')
  await page.getByLabel('用户名', { exact: true }).fill(username)
  await page.getByLabel('密码', { exact: true }).fill(password)
  await page.getByRole('checkbox').setChecked(true)
  await page.getByRole('button', { name: '登录', exact: true }).click()
  await expect(page).toHaveURL('/')
}

test('部署目标：创建、检查、启停、删除与管理员限制', async ({ page }) => {
  await login(page)
  // 唯一名称，避免跨运行残留（数据库每次运行前重建）。
  const name = `target-${Date.now()}`

  // 通过真实向导创建目标：预发布、回环地址（允许不带凭据）、/healthz。
  await page.goto('/deploy-targets')
  await page.getByRole('link', { name: '新建部署目标' }).click()
  await expect(page).toHaveURL('/deploy-targets/new')
  await page.getByLabel('名称').fill(name)
  await page.getByLabel('环境').selectOption('staging')
  await page.getByLabel('端点').fill('http://127.0.0.1:8080')
  await page.getByLabel('健康路径').fill('/healthz')
  await page.getByRole('button', { name: '创建', exact: true }).click()
  await expect(page).toHaveURL('/deploy-targets')

  const row = page.getByRole('row', { name: new RegExp(name) })
  await expect(row).toBeVisible()
  await expect(row.getByText('预发布')).toBeVisible()

  // 列表行检查连通性：E2E 后端自身 /healthz 返回 2xx。
  await row.getByRole('button', { name: '检查', exact: true }).click()
  await expect(row.getByText(/健康/)).toBeVisible()

  // 详情页：禁用后再启用，状态徽章随之变化。
  await row.getByRole('link', { name: new RegExp(name) }).click()
  await expect(page).toHaveURL(/\/deploy-targets\/[A-Za-z0-9_-]{16,64}$/)
  await expect(page.getByRole('heading', { name: new RegExp(name) })).toBeVisible()
  await page.getByLabel('状态').selectOption('disabled')
  await page.getByRole('button', { name: '保存修改' }).click()
  await expect(page.getByLabel('状态')).toHaveValue('disabled')
  await expect(page.locator('span').filter({ hasText: /^已禁用$/ })).toBeVisible()
  await page.getByLabel('状态').selectOption('active')
  await page.getByRole('button', { name: '保存修改' }).click()
  await expect(page.getByLabel('状态')).toHaveValue('active')
  await expect(page.locator('span').filter({ hasText: /^启用$/ })).toBeVisible()

  // 删除后回到列表：该行消失。断言只针对本用例创建的行，因为同一测试库可能
  // 保留早前运行创建的其他目标（空状态只在全新数据库时成立）。
  await page.getByRole('button', { name: '删除', exact: true }).click()
  await page.getByRole('button', { name: '确认删除' }).click()
  await expect(page).toHaveURL('/deploy-targets')
  await expect(page.getByRole('row', { name: new RegExp(name) })).toHaveCount(0)

  // 普通成员：管理接口 403，直接访问管理员页面得到无权访问，且没有创建入口。
  const memberName = `target-viewer-${Date.now()}`
  await page.goto('/settings')
  await page.getByRole('button', { name: '创建用户' }).click()
  await page.getByLabel('用户名').fill(memberName)
  await page.getByLabel('显示名').fill('部署目标只读')
  await page.getByLabel('初始密码').fill('Target-viewer-password-2026')
  await page.getByLabel('角色').selectOption('member')
  await page.getByRole('button', { name: '创建', exact: true }).click()
  await expect(page.getByRole('row', { name: new RegExp(memberName) })).toBeVisible()

  const browser = page.context().browser()!
  const memberCtx = await browser.newContext()
  const member = await memberCtx.newPage()
  await login(member, memberName, 'Target-viewer-password-2026')

  await member.goto('/deploy-targets')
  await expect(member.getByText('无权访问')).toBeVisible()
  await expect(member.getByRole('link', { name: '新建部署目标' })).toHaveCount(0)
  // 管理员专属页面在成员侧边栏中不出现（路由守卫与服务端仍各自强制）。
  await expect(member.locator('aside').getByRole('link', { name: '部署目标' })).toHaveCount(0)

  const status = await member.evaluate(async () => {
    const { apiRequest } = await import('/src/stores/session.ts')
    try {
      await apiRequest('/deployment-targets')
      return 200
    } catch (error) {
      return (error as { status: number }).status
    }
  })
  expect(status).toBe(403)
  await memberCtx.close()
})
