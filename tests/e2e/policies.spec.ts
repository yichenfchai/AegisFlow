import { test, expect } from '@playwright/test'
import type { APIRequestContext, Page } from '@playwright/test'
import { createServer, type Server } from 'node:http'
import type { AddressInfo } from 'node:net'
import { readFile } from 'node:fs/promises'
import { E2E_INGEST_WEBHOOK_SECRET, signIngestBody } from './ingestWebhook'

const admin = { username: 'e2e-admin', password: 'E2e-admin-password-2026' }
// 机器接口（签名扫描回写）不走浏览器会话：直接 POST 后端，避免经过 Vite 代理。
const backendURL = 'http://127.0.0.1:8080'

let upstream: Server
let upstreamPort = 0

test.beforeAll(async () => {
  // 部署执行需要一个返回 2xx 的上游；用本 spec 内的 Node stub 代替真实服务。
  upstream = createServer((req, res) => {
    req.resume()
    req.on('end', () => {
      res.writeHead(200, { 'Content-Type': 'text/plain' })
      res.end('policy deploy ok\n')
    })
  })
  await new Promise<void>((resolve) => upstream.listen(0, '127.0.0.1', resolve))
  upstreamPort = (upstream.address() as AddressInfo).port
})

test.afterAll(async () => {
  await new Promise<void>((resolve) => {
    upstream.close(() => resolve())
    // Go 的 HTTP client 会保留 keep-alive 连接；主动断开避免 Playwright 进程等待。
    upstream.closeAllConnections()
  })
})

async function login(page: Page) {
  await page.goto('/login')
  await page.getByLabel('用户名', { exact: true }).fill(admin.username)
  await page.getByLabel('密码', { exact: true }).fill(admin.password)
  await page.getByRole('checkbox').setChecked(true)
  await page.getByRole('button', { name: '登录', exact: true }).click()
  await expect(page).toHaveURL('/')
}

/** 通过真实接口创建项目（策略/扫描用例聚焦在被测页面，不重复项目向导用例）。 */
async function createProjectAPI(page: Page, name: string): Promise<string> {
  return page.evaluate(async (projectName) => {
    const { apiRequest } = await import('/src/stores/session.ts')
    const project = await apiRequest<{ id: string }>('/projects', {
      method: 'POST',
      body: JSON.stringify({ name: projectName, buildCommand: 'echo policy-e2e', visibility: 'public' }),
    })
    return project.id
  }, name)
}

/** 通过真实 UI 创建项目并返回 id，用于覆盖「创建项目 → 设置策略」的完整操作路径。 */
async function createProjectUI(page: Page, name: string): Promise<string> {
  await page.goto('/')
  await page.getByRole('link', { name: '创建项目' }).click()
  await page.getByLabel('项目名称').fill(name)
  await page.getByLabel('构建命令').fill('echo policy-e2e')
  await page.getByRole('button', { name: '创建项目', exact: true }).click()
  await expect(page).toHaveURL('/')
  await expect(page.getByRole('row', { name: new RegExp(name) })).toBeVisible()
  const projectId = await page.evaluate(async (projectName) => {
    const { apiRequest } = await import('/src/stores/session.ts')
    const result = await apiRequest<{ items: { id: string; name: string }[] }>(
      '/projects?limit=100&offset=0',
    )
    return result.items.find((item) => item.name === projectName)?.id ?? ''
  }, name)
  expect(projectId).not.toBe('')
  return projectId
}

async function createStagingTarget(page: Page, name: string): Promise<string> {
  return page.evaluate(
    async ({ targetName, port }) => {
      const { apiRequest } = await import('/src/stores/session.ts')
      const target = await apiRequest<{ id: string }>('/deployment-targets', {
        method: 'POST',
        body: JSON.stringify({
          name: targetName,
          environment: 'staging',
          endpoint: `http://127.0.0.1:${port}`,
        }),
      })
      return target.id
    },
    { targetName: name, port: upstreamPort },
  )
}

async function createReview(page: Page, projectId: string, subject: string): Promise<string> {
  return page.evaluate(
    async ({ id, reviewSubject }) => {
      const { apiRequest } = await import('/src/stores/session.ts')
      const review = await apiRequest<{ id: string }>(
        `/projects/${encodeURIComponent(id)}/reviews`,
        { method: 'POST', body: JSON.stringify({ subject: reviewSubject, version: 'v1.0.0' }) },
      )
      return review.id
    },
    { id: projectId, reviewSubject: subject },
  )
}

async function approveReview(page: Page, reviewId: string, reason: string) {
  await page.evaluate(
    async ({ id, approveReason }) => {
      const { apiRequest } = await import('/src/stores/session.ts')
      await apiRequest(`/reviews/${encodeURIComponent(id)}/approve`, {
        method: 'POST',
        body: JSON.stringify({ reason: approveReason }),
      })
    },
    { id: reviewId, approveReason: reason },
  )
}

/** 以外部扫描工具身份签名回写一条扫描结论（HMAC + 时间戳 + delivery 幂等）。 */
async function postSignedScan(
  request: APIRequestContext,
  projectId: string,
  verdict: 'passed' | 'failed',
  delivery: string,
) {
  const body = JSON.stringify({
    source: 'e2e-policy-scanner',
    tool: 'trivy',
    version: '0.55.0',
    verdict,
    summary: `策略门禁扫描 ${verdict}`,
  })
  const timestamp = String(Math.floor(Date.now() / 1000))
  const response = await request.post(`${backendURL}/api/v1/projects/${projectId}/scans`, {
    headers: {
      'Content-Type': 'application/json',
      'X-AegisFlow-Timestamp': timestamp,
      'X-AegisFlow-Signature': signIngestBody(E2E_INGEST_WEBHOOK_SECRET, timestamp, body),
      'X-AegisFlow-Delivery': delivery,
    },
    data: body,
  })
  expect(response.status(), await response.text()).toBe(201)
}

async function expectDeploymentCreated(page: Page) {
  await expect(page).toHaveURL(/\/deploys\/[A-Za-z0-9_-]{16,64}$/)
  await expect(page.getByText('成功', { exact: true }).first()).toBeVisible({ timeout: 20_000 })
}

test('项目策略审查门禁：requireReview=true 时部署必须关联已批准审查', async ({ page }) => {
  test.setTimeout(180_000)
  await login(page)
  const suffix = Date.now()
  const projectName = `policy-review-${suffix}`
  const projectId = await createProjectUI(page, projectName)

  // 项目列表 → 策略子页面；勾选「关联已批准审查」并保存。
  await page.getByRole('row', { name: new RegExp(projectName) }).getByRole('link', { name: '策略' }).click()
  await expect(page).toHaveURL(`/projects/${projectId}/policy`)
  await expect(
    page.getByRole('heading', { level: 1, name: `${projectName} · 自动化策略` }),
  ).toBeVisible()
  await page.getByLabel('关联已批准审查').check()
  await page.getByRole('button', { name: '保存策略' }).click()
  await expect(page.getByText(/自动化策略已保存/)).toBeVisible()
  await expect(page.getByText('要求', { exact: true })).toBeVisible()

  const targetId = await createStagingTarget(page, `policy-review-target-${suffix}`)

  // 部署表单出现中文策略提示；未关联审查创建会得到后端 400 → 中文错误。
  await page.goto('/deploys/new')
  await page.getByLabel('项目').selectOption(projectId)
  await expect(page.getByText(/该项目策略要求关联已批准审查/).first()).toBeVisible({ timeout: 20_000 })
  await page.getByLabel('目标').selectOption(targetId)
  await page.getByLabel('版本').fill('v1.0.0')
  await page.getByRole('button', { name: '创建部署', exact: true }).click()
  await expect(page.getByRole('alert')).toContainText('请求不合法')

  // 通过真实接口发起并批准一条同项目审查，再回到表单选择它创建部署。
  const reviewId = await createReview(page, projectId, `策略门禁审查 ${suffix}`)
  await approveReview(page, reviewId, '策略门禁部署前确认')
  await page.goto('/deploys/new')
  await page.getByLabel('项目').selectOption(projectId)
  await expect(page.getByLabel('关联网关审查')).toContainText('#', { timeout: 20_000 })
  await page.getByLabel('关联网关审查').selectOption(reviewId)
  await page.getByLabel('目标').selectOption(targetId)
  await page.getByLabel('版本').fill('v1.0.0')
  await page.getByRole('button', { name: '创建部署', exact: true }).click()
  await expectDeploymentCreated(page)
})

test('项目策略扫描门禁：最新扫描必须通过，签名回写 passed 后部署成功', async ({ page, request }) => {
  test.setTimeout(180_000)
  await login(page)
  const suffix = Date.now()
  const projectName = `policy-scan-${suffix}`
  const projectId = await createProjectAPI(page, projectName)

  // 策略页：不要求审查（默认关闭），最新扫描结论选择「必须通过」。
  await page.goto(`/projects/${projectId}/policy`)
  await expect(
    page.getByRole('heading', { level: 1, name: `${projectName} · 自动化策略` }),
  ).toBeVisible({ timeout: 20_000 })
  await expect(page.getByLabel('关联已批准审查')).not.toBeChecked()
  await page.getByLabel('最新扫描结论').selectOption('passed')
  await page.getByRole('button', { name: '保存策略' }).click()
  await expect(page.getByText(/自动化策略已保存/)).toBeVisible()

  const targetId = await createStagingTarget(page, `policy-scan-target-${suffix}`)

  // 无扫描：表单提示策略要求，创建被后端 400 拒绝。
  await page.goto('/deploys/new')
  await page.getByLabel('项目').selectOption(projectId)
  await expect(page.getByText(/该项目策略要求最新扫描必须通过/)).toBeVisible({ timeout: 20_000 })
  await page.getByLabel('目标').selectOption(targetId)
  await page.getByLabel('版本').fill('v2.0.0')
  await page.getByRole('button', { name: '创建部署', exact: true }).click()
  await expect(page.getByRole('alert')).toContainText('请求不合法')
  await expect(page.getByRole('alert')).toContainText('扫描')

  // 用既有签名回写通道登记一条 passed 扫描，然后重试创建部署。
  await postSignedScan(request, projectId, 'passed', `e2e-policy-scan-${suffix}`)
  await page.goto('/deploys/new')
  await page.getByLabel('项目').selectOption(projectId)
  await expect(page.getByText(/该项目策略要求最新扫描必须通过/)).toBeVisible({ timeout: 20_000 })
  await page.getByLabel('目标').selectOption(targetId)
  await page.getByLabel('版本').fill('v2.0.0')
  await page.getByRole('button', { name: '创建部署', exact: true }).click()
  await expectDeploymentCreated(page)
})

test('审计 CSV 导出：工具栏按钮触发浏览器下载且表头与筛选正确', async ({ page }) => {
  test.setTimeout(120_000)
  await login(page)
  await page.goto('/settings')
  await page.getByRole('button', { name: '审计', exact: true }).click()

  // 用「登录」动作筛选，导出的行必须全部是 auth.login，且表头是固定列。
  await page.getByLabel('动作', { exact: true }).selectOption('auth.login')
  await expect(page.locator('tbody tr').first()).toBeVisible({ timeout: 20_000 })
  const [download] = await Promise.all([
    page.waitForEvent('download'),
    page.getByRole('button', { name: '导出 CSV' }).click(),
  ])
  expect(download.suggestedFilename()).toMatch(/^audit-\d{8}-\d{6}\.csv$/)
  const filePath = await download.path()
  expect(filePath).not.toBeNull()
  const content = await readFile(filePath!, 'utf8')
  const lines = content.split('\r\n').filter((line) => line !== '')
  expect(lines[0]).toBe('id,createdAt,action,objectType,objectId,result,actor,detail')
  expect(lines.length).toBeGreaterThan(1)
  for (const line of lines.slice(1)) {
    expect(line.split(',')[2]).toBe('auth.login')
  }
  await expect(page.getByText(/已开始下载当前筛选条件下的审计 CSV/)).toBeVisible()
})
