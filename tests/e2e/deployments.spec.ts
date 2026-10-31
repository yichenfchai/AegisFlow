import { test, expect } from '@playwright/test'
import type { Page } from '@playwright/test'
import { createServer, type Server } from 'node:http'
import type { AddressInfo } from 'node:net'

const admin = { username: 'e2e-admin', password: 'E2e-admin-password-2026' }

let upstream: Server
let upstreamPort = 0

test.beforeAll(async () => {
  upstream = createServer((req, res) => {
    req.resume()
    req.on('end', () => {
      res.writeHead(200, { 'Content-Type': 'text/plain' })
      res.end('deploy-line-one\ndeploy-line-two\n')
    })
  })
  await new Promise<void>((resolve) => upstream.listen(0, '127.0.0.1', resolve))
  upstreamPort = (upstream.address() as AddressInfo).port
})

test.afterAll(async () => {
  await new Promise<void>((resolve) => {
    upstream.close(() => resolve())
    // The Go HTTP client keeps idle connections alive; drop them so the
    // Playwright process can exit without waiting for the keep-alive timeout.
    upstream.closeAllConnections()
  })
})

async function login(page: Page, username = admin.username, password = admin.password) {
  await page.goto('/login')
  await page.getByLabel('用户名', { exact: true }).fill(username)
  await page.getByLabel('密码', { exact: true }).fill(password)
  await page.getByRole('checkbox').setChecked(true)
  await page.getByRole('button', { name: '登录', exact: true }).click()
  await expect(page).toHaveURL('/')
}

async function createTarget(page: Page, name: string, environment: 'staging' | 'production') {
  return page.evaluate(
    async ({ name, environment, port }) => {
      const { apiRequest } = await import('/src/stores/session.ts')
      const target = await apiRequest<{ id: string }>('/deployment-targets', {
        method: 'POST',
        body: JSON.stringify({ name, environment, endpoint: `http://127.0.0.1:${port}` }),
      })
      return target.id
    },
    { name, environment, port: upstreamPort },
  )
}

async function createDeployment(page: Page, projectId: string, targetId: string, version: string) {
  await page.goto('/deploys/new')
  await page.getByLabel('项目').selectOption(projectId)
  await page.getByLabel('目标').selectOption(targetId)
  await page.getByLabel('版本').fill(version)
  await page.getByRole('button', { name: '创建部署', exact: true }).click()
  await expect(page).toHaveURL(/\/deploys\/[A-Za-z0-9_-]{16,64}$/)
  return new URL(page.url()).pathname.split('/').pop()!
}

async function expectStatus(page: Page, text: string) {
  await expect(page.getByText(text, { exact: true }).first()).toBeVisible({ timeout: 20_000 })
}

test('部署与审批闭环：部署、审批、回滚与管理员限制', async ({ page }) => {
  test.setTimeout(120_000)
  await login(page)
  const suffix = Date.now()
  const projectName = `deploy-proj-${suffix}`

  // 通过 UI 创建项目。
  await page.goto('/')
  await page.getByRole('link', { name: '创建项目' }).click()
  await page.getByLabel('项目名称').fill(projectName)
  await page.getByLabel('构建命令').fill('echo deploy-e2e')
  await page.getByRole('button', { name: '创建项目', exact: true }).click()
  await expect(page).toHaveURL('/')
  await expect(page.getByRole('row', { name: new RegExp(projectName) })).toBeVisible()

  const projectId = await page.evaluate(async (name) => {
    const { apiRequest } = await import('/src/stores/session.ts')
    const result = await apiRequest<{ items: { id: string; name: string }[] }>('/projects?limit=100&offset=0')
    return result.items.find((item) => item.name === name)?.id ?? ''
  }, projectName)
  expect(projectId).not.toBe('')

  const stagingTargetId = await createTarget(page, `deploy-staging-${suffix}`, 'staging')
  const productionTargetId = await createTarget(page, `deploy-production-${suffix}`, 'production')

  // 预发布第一次部署：直接排队执行，日志来自本 spec 的 Node stub。
  await createDeployment(page, projectId, stagingTargetId, 'v1.2.3')
  await expectStatus(page, '成功')
  await expect(page.getByText(/deploy-line-one/)).toBeVisible({ timeout: 20_000 })
  await expect(page.getByText(/deploy-line-two/)).toBeVisible()

  // 预发布第二次部署：成功后回滚，会创建一条 v1.2.3 的新部署并标记回滚来源。
  await createDeployment(page, projectId, stagingTargetId, 'v1.2.4')
  await expectStatus(page, '成功')
  await page.getByRole('button', { name: '回滚', exact: true }).click()
  await page.getByRole('button', { name: '确认回滚' }).click()
  await expect(page).toHaveURL(/\/deploys\/[A-Za-z0-9_-]{16,64}$/)
  await expectStatus(page, '成功')
  await expect(page.getByText('v1.2.3', { exact: true })).toBeVisible()
  await expect(page.getByText(/回滚自 #\d+/)).toBeVisible()

  // 生产部署：进入等待审批，管理员批准后执行成功。
  const productionApprove = await createDeployment(page, projectId, productionTargetId, 'v2.0.0')
  await expectStatus(page, '等待审批')
  await page.goto('/approvals')
  await page.getByLabel('状态', { exact: true }).selectOption('pending')
  const pendingRow = page.locator('tbody tr').first()
  await expect(pendingRow).toBeVisible()
  await expect(pendingRow.getByText('待审批')).toBeVisible()
  await pendingRow.getByRole('button', { name: '批准', exact: true }).click()
  await page.getByRole('button', { name: '确认批准' }).click()
  await expect(page.getByText('没有待处理的审批')).toBeVisible()
  await page.goto(`/deploys/${productionApprove}`)
  await expectStatus(page, '成功')
  await expect(page.getByText(/deploy-line-one/)).toBeVisible({ timeout: 20_000 })

  // 生产部署：拒绝后部署变为已拒绝，审批行保留理由。
  const productionReject = await createDeployment(page, projectId, productionTargetId, 'v2.0.1')
  await expectStatus(page, '等待审批')
  await page.goto('/approvals')
  await page.getByLabel('状态', { exact: true }).selectOption('pending')
  const rejectRow = page.locator('tbody tr').first()
  await expect(rejectRow).toBeVisible()
  await rejectRow.getByRole('button', { name: '拒绝', exact: true }).click()
  await page.getByLabel('拒绝理由').fill('变更需拆分')
  await page.getByRole('button', { name: '确认拒绝' }).click()
  await page.getByLabel('状态', { exact: true }).selectOption('rejected')
  const rejectedRow = page.getByRole('row', { name: /变更需拆分/ })
  await expect(rejectedRow).toBeVisible()
  await expect(rejectedRow.getByText('已拒绝')).toBeVisible()
  await page.goto(`/deploys/${productionReject}`)
  await expectStatus(page, '已拒绝')

  // 全局部署列表：服务端分页 + 项目/状态筛选。
  await page.goto('/deploys')
  await expect(page.getByRole('row', { name: new RegExp(projectName) }).first()).toBeVisible()
  await page.getByLabel('状态', { exact: true }).selectOption('rejected')
  const rejectedDeployment = page.getByRole('row', { name: new RegExp(projectName) }).first()
  await expect(rejectedDeployment).toBeVisible()
  await expect(rejectedDeployment.getByText('已拒绝')).toBeVisible()
  await page.getByLabel('项目', { exact: true }).selectOption(projectId)
  await expect(page.getByRole('row', { name: new RegExp(projectName) }).first()).toBeVisible()

  // 普通成员：侧边栏没有部署/审批入口，直连页面得到无权访问，管理接口 403。
  const memberName = `deploy-viewer-${suffix}`
  await page.goto('/settings')
  await page.getByRole('button', { name: '创建用户' }).click()
  await page.getByLabel('用户名').fill(memberName)
  await page.getByLabel('显示名').fill('部署只读')
  await page.getByLabel('初始密码').fill('Deploy-viewer-password-2026')
  await page.getByLabel('角色').selectOption('member')
  await page.getByRole('button', { name: '创建', exact: true }).click()
  await expect(page.getByRole('row', { name: new RegExp(memberName) })).toBeVisible()

  const browser = page.context().browser()!
  const memberCtx = await browser.newContext()
  const member = await memberCtx.newPage()
  await login(member, memberName, 'Deploy-viewer-password-2026')
  await expect(member.getByRole('link', { name: '项目列表' })).toBeVisible()
  await expect(member.getByRole('link', { name: '部署记录' })).toHaveCount(0)
  await expect(member.getByRole('link', { name: '审批中心' })).toHaveCount(0)
  await member.goto('/deploys')
  await expect(member.getByText('无权访问')).toBeVisible()

  const denied = await member.evaluate(async () => {
    const { apiRequest } = await import('/src/stores/session.ts')
    try {
      await apiRequest('/deployments')
      return 200
    } catch (error) {
      return (error as { status: number }).status
    }
  })
  expect(denied).toBe(403)
  await memberCtx.close()
})
