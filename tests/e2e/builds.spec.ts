import { test, expect } from '@playwright/test'
import type { Page } from '@playwright/test'

const admin = { username: 'e2e-admin', password: 'E2e-admin-password-2026' }

/**
 * 构建命令在 POSIX 由 `sh -c`、在 Windows 由 `cmd /c` 执行。Go 的 os/exec 会
 * 为 cmd.exe 转义内嵌双引号，而 cmd 不认这种转义：整段 -e 代码会以带引号的
 * 字符串字面量到达 node 并立即退出。因此 Windows 用无引号形式（cmd 的括号
 * 在简单命令里按字面传递），其他平台保留带引号的等价写法；两者运行同一段
 * node 程序，用 node 而不是平台专有命令，避免依赖 sleep/ping 的差异。
 */
const longRunningCommand =
  process.platform === 'win32' ? 'node -e setTimeout(function(){},15000)' : `node -e "setTimeout(function(){},15000)"`
const failingCommand =
  process.platform === 'win32' ? 'node -e process.exit(3)' : `node -e "process.exit(3)"`

async function login(page: Page, username = admin.username, password = admin.password) {
  await page.goto('/login')
  await page.getByLabel('用户名', { exact: true }).fill(username)
  await page.getByLabel('密码', { exact: true }).fill(password)
  await page.getByRole('checkbox').setChecked(true)
  await page.getByRole('button', { name: '登录', exact: true }).click()
  await expect(page).toHaveURL('/')
}

/** 通过真实向导创建带构建命令的项目（唯一名称，避免上一轮残留）。 */
async function createProjectWithCommand(page: Page, name: string, command: string) {
  await page.goto('/')
  await page.getByRole('link', { name: '创建项目' }).click()
  await page.getByLabel('项目名称').fill(name)
  await page.getByLabel('构建命令').fill(command)
  await page.getByRole('button', { name: '创建项目', exact: true }).click()
  await expect(page).toHaveURL('/')
  const projectRow = page.getByRole('row', { name: new RegExp(name) })
  await expect(projectRow.getByText('活跃')).toBeVisible()
  await projectRow.getByRole('button', { name: '构建', exact: true }).click()
  await expect(page).toHaveURL('/builds')
}

/** 打开该项目的第一次构建详情（新项目 BuildNumber 恒为 1）。 */
async function openFirstBuild(page: Page, name: string) {
  const buildRow = page.getByRole('row', { name: new RegExp(name) })
  await expect(buildRow).toBeVisible()
  await buildRow.getByRole('link', { name: '#1' }).click()
  await expect(page).toHaveURL(/\/builds\/[A-Za-z0-9_-]{16,64}$/)
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

  // 终态构建可以重试：新构建沿用原分支与项目当前命令，并跳到新构建详情。
  await page.getByRole('button', { name: '重试构建' }).click()
  await expect(page.getByRole('heading', { name: '构建 #2' })).toBeVisible({ timeout: 20_000 })
  await expect(page.getByText('成功', { exact: true })).toBeVisible({ timeout: 20_000 })
  await expect(page.getByRole('button', { name: '重试构建' })).toBeVisible()

  // 触发构建写入持久化审计，管理员可在审计页按中文动作看到。断言限定在表格内：
  // 审计筛选下拉里也有同名动作选项，直接 getByText 会先命中隐藏的 <option>。
  await page.goto('/settings')
  await page.getByRole('button', { name: '审计', exact: true }).click()
  const auditTable = page.getByRole('table')
  await expect(auditTable.getByText('触发构建').first()).toBeVisible()
  await expect(auditTable.getByText(/构建:/).first()).toBeVisible()

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
  // 重试后该项目有两行构建，取第一行即可断言可见性。
  await expect(viewer.getByRole('row', { name: new RegExp(name) }).first()).toBeVisible()
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

test('构建运行中取消：状态变为已取消并写入取消日志', async ({ page }) => {
  await login(page)
  const name = `e2e-cancel-${Date.now()}`
  await createProjectWithCommand(page, name, longRunningCommand)
  await openFirstBuild(page, name)

  // 真实 node 长任务先启动为运行中，再通过 UI 取消；不使用固定 sleep。
  await expect(page.getByText('运行中', { exact: true })).toBeVisible({ timeout: 20_000 })
  await page.getByRole('button', { name: '取消构建' }).click()
  await expect(page.getByText('已取消', { exact: true })).toBeVisible({ timeout: 20_000 })
  // worker 在取消后写入系统日志（build cancelled），SSE 尾部 flush 后可见。
  await expect(page.getByText('build cancelled', { exact: true })).toBeVisible({ timeout: 20_000 })
  // 终态构建不再提供取消入口。
  await expect(page.getByRole('button', { name: '取消构建' })).toHaveCount(0)
})

test('构建失败：非零退出码记录为失败并带退出码说明', async ({ page }) => {
  await login(page)
  const name = `e2e-fail-${Date.now()}`
  await createProjectWithCommand(page, name, failingCommand)
  await openFirstBuild(page, name)

  await expect(page.getByText('失败', { exact: true })).toBeVisible({ timeout: 20_000 })
  // 失败日志来自真实进程退出码，不是伪造的演示文案。
  await expect(page.getByText('build failed: exit status 3', { exact: true })).toBeVisible({ timeout: 20_000 })
})
