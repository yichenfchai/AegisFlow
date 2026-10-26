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

test('构建闭环：触发、成功日志与 viewer 只读', async ({ page }) => {
  await login(page)
  // 唯一项目名，避免上一轮 E2E 残留（数据库每次运行前重建）。
  const name = `e2e-build-${Date.now()}`

  // 通过真实向导创建带构建命令的项目。
  await page.goto('/')
  await page.getByRole('link', { name: '创建项目' }).click()
  await page.getByLabel('项目名称').fill(name)
  await page.getByLabel('构建命令').fill('echo e2e-build-ok')
  await page.getByRole('button', { name: '创建项目', exact: true }).click()
  await expect(page).toHaveURL('/')
  const projectRow = page.getByRole('row', { name: new RegExp(name) })
  await expect(projectRow.getByText('活跃')).toBeVisible()

  // 项目行触发构建后跳转构建列表。
  await projectRow.getByRole('button', { name: '构建', exact: true }).click()
  await expect(page).toHaveURL('/builds')
  const buildRow = page.getByRole('row', { name: new RegExp(name) })
  await expect(buildRow).toBeVisible()
  // 详情页用带 Bearer 的 SSE 流式读取日志（EventSource 无法带 Authorization）。
  const eventStream = page.waitForResponse(
    (response) => response.url().includes('/api/v1/builds/') && response.url().includes('/events'),
  )
  await buildRow.getByRole('link', { name: '#1' }).click()
  const streamResponse = await eventStream
  expect(streamResponse.status()).toBe(200)
  expect(streamResponse.headers()['content-type']).toContain('text/event-stream')
  await expect(page).toHaveURL(/\/builds\/[A-Za-z0-9_-]{16,64}$/)

  // 详情页展示命令快照，并通过 SSE 推送到达成功终态与日志。
  await expect(page.getByText('echo e2e-build-ok').first()).toBeVisible()
  await expect(page.getByText('成功', { exact: true })).toBeVisible({ timeout: 20_000 })
  // 日志按游标累积：进程输出与收尾系统行都要出现。
  await expect(page.getByText('e2e-build-ok', { exact: true })).toBeVisible({ timeout: 20_000 })
  await expect(page.getByText('build succeeded', { exact: true })).toBeVisible({ timeout: 20_000 })
  // 终态构建没有取消按钮。
  await expect(page.getByRole('button', { name: '取消构建' })).toHaveCount(0)

  const projectID = await page.evaluate(async (projectName) => {
    const { apiRequest } = await import('/src/stores/session.ts')
    const result = await apiRequest<{ items: { id: string; name: string }[] }>('/projects?limit=100&offset=0')
    return result.items.find((item) => item.name === projectName)?.id ?? ''
  }, name)
  expect(projectID).not.toBe('')

  // viewer 只读：能看到构建列表，但触发构建被服务端拒绝。
  const username = `build-viewer-${Date.now()}`
  await page.evaluate(async ({ username }) => {
    const { apiRequest } = await import('/src/stores/session.ts')
    return apiRequest('/users', {
      method: 'POST',
      body: JSON.stringify({ username, password: 'Viewer-pass-2026', displayName: '构建只读', role: 'viewer' }),
    })
  }, { username })

  const browser = page.context().browser()!
  const viewerCtx = await browser.newContext()
  const viewer = await viewerCtx.newPage()
  await login(viewer, username, 'Viewer-pass-2026')
  await viewer.goto('/builds')
  await expect(viewer.getByRole('row', { name: new RegExp(name) })).toBeVisible()
  const status = await viewer.evaluate(async (id) => {
    const { apiRequest } = await import('/src/stores/session.ts')
    try {
      await apiRequest(`/projects/${id}/builds`, { method: 'POST', body: '{}' })
      return 201
    } catch (error) {
      return (error as { status: number }).status
    }
  }, projectID)
  expect(status).toBe(403)
  await viewerCtx.close()
})
