import { test, expect } from '@playwright/test'
import type { APIRequestContext, Page } from '@playwright/test'
import { E2E_INGEST_WEBHOOK_SECRET, signIngestBody } from './ingestWebhook'

const admin = { username: 'e2e-admin', password: 'E2e-admin-password-2026' }
// 机器接口不走浏览器会话：直接对后端签名 POST，避免经过 Vite 代理。
const backendURL = 'http://127.0.0.1:8080'

async function login(page: Page, username = admin.username, password = admin.password) {
  await page.goto('/login')
  await page.getByLabel('用户名', { exact: true }).fill(username)
  await page.getByLabel('密码', { exact: true }).fill(password)
  await page.getByRole('checkbox').setChecked(true)
  await page.getByRole('button', { name: '登录', exact: true }).click()
  await expect(page).toHaveURL('/')
}

async function createProject(page: Page, name: string): Promise<string> {
  return page.evaluate(async (name) => {
    const { apiRequest } = await import('/src/stores/session.ts')
    const project = await apiRequest<{ id: string }>('/projects', {
      method: 'POST',
      body: JSON.stringify({ name, buildCommand: 'echo scans-defense-e2e', visibility: 'public' }),
    })
    return project.id
  }, name)
}

async function createUser(page: Page, username: string, role: 'member' | 'viewer') {
  await page.evaluate(
    async ({ username, role }) => {
      const { apiRequest } = await import('/src/stores/session.ts')
      await apiRequest('/users', {
        method: 'POST',
        body: JSON.stringify({ username, password: 'Defense-pass-2026', displayName: username, role }),
      })
    },
    { username, role },
  )
}

/**
 * 以外部工具身份签名回写一条交付：Node `crypto` 计算 HMAC-SHA256，Playwright
 * `request` 直接 POST 后端机器接口，不依赖任何真实外部服务。
 */
async function postSigned(
  request: APIRequestContext,
  path: string,
  body: string,
  delivery: string,
  expectedStatus: number,
) {
  const timestamp = String(Math.floor(Date.now() / 1000))
  const response = await request.post(`${backendURL}${path}`, {
    headers: {
      'Content-Type': 'application/json',
      'X-AegisFlow-Timestamp': timestamp,
      'X-AegisFlow-Signature': signIngestBody(E2E_INGEST_WEBHOOK_SECRET, timestamp, body),
      'X-AegisFlow-Delivery': delivery,
    },
    data: body,
  })
  expect(response.status(), await response.text()).toBe(expectedStatus)
}

test('安全扫描与攻击防御：签名回写登记、筛选与详情、admin 确认事件、非 admin 受限', async ({
  page,
  request,
}) => {
  test.setTimeout(180_000)
  await login(page)
  const suffix = Date.now()
  const projectName = `scans-defense-${suffix}`
  const projectId = await createProject(page, projectName)

  // ---- 安全扫描：外部工具签名回写 ----
  const passedBody = JSON.stringify({
    source: 'e2e-scanner',
    tool: 'trivy',
    version: '0.55.0',
    verdict: 'passed',
    summary: '静态扫描通过：无高危漏洞\n共 0 个问题',
    reportUrl: 'https://example.com/scan/report-1',
  })
  const passedDelivery = `e2e-scan-passed-${suffix}`
  await postSigned(request, `/api/v1/projects/${projectId}/scans`, passedBody, passedDelivery, 201)
  // 同一 delivery 重放：幂等 200，不新增记录。
  await postSigned(request, `/api/v1/projects/${projectId}/scans`, passedBody, passedDelivery, 200)

  // 侧栏入口进入真实的安全扫描页，按项目筛选到刚回写的记录。
  await page.locator('aside').getByRole('link', { name: '安全扫描' }).click()
  await expect(page).toHaveURL('/scans')
  await expect(page.getByRole('heading', { level: 1, name: '安全扫描' })).toBeVisible()
  await page.getByLabel('项目', { exact: true }).selectOption(projectId)
  const passedRow = page.getByRole('row').filter({ hasText: projectName })
  await expect(passedRow).toBeVisible({ timeout: 20_000 })
  await expect(passedRow.getByText('通过', { exact: true })).toBeVisible()
  await expect(passedRow).toContainText('e2e-scanner')
  await expect(passedRow).toContainText('trivy · 0.55.0')

  // 详情：来源、工具、摘要（保留换行）、报告外链新标签打开。
  await passedRow.getByRole('link', { name: '详情' }).click()
  await expect(page).toHaveURL(/\/scans\/[A-Za-z0-9_-]{16,64}$/)
  await expect(page.getByRole('heading', { level: 1, name: '扫描详情' })).toBeVisible()
  await expect(page.getByText('e2e-scanner', { exact: true })).toBeVisible()
  await expect(page.getByText(/静态扫描通过：无高危漏洞/)).toContainText('共 0 个问题')
  const reportLink = page.getByRole('link', { name: '查看外部扫描报告' })
  await expect(reportLink).toHaveAttribute('href', 'https://example.com/scan/report-1')
  await expect(reportLink).toHaveAttribute('target', '_blank')
  await expect(reportLink).toHaveAttribute('rel', /noopener noreferrer/)

  // 第二条 failed：徽章「失败」且 verdict 筛选生效。
  const failedBody = JSON.stringify({
    source: 'e2e-scanner',
    tool: 'grype',
    verdict: 'failed',
    summary: '发现 3 个高危漏洞',
  })
  await postSigned(request, `/api/v1/projects/${projectId}/scans`, failedBody, `e2e-scan-failed-${suffix}`, 201)
  await page.goto('/scans')
  await page.getByLabel('项目', { exact: true }).selectOption(projectId)
  await page.getByLabel('结论', { exact: true }).selectOption('failed')
  const failedRow = page.getByRole('row').filter({ hasText: '发现 3 个高危漏洞' })
  await expect(failedRow).toBeVisible()
  await expect(failedRow.getByText('失败', { exact: true })).toBeVisible()
  await page.getByLabel('结论', { exact: true }).selectOption('passed')
  await expect(page.getByRole('row').filter({ hasText: '发现 3 个高危漏洞' })).toHaveCount(0)

  // ---- 攻击防御：签名回写事件 + admin 确认 ----
  const eventBody = JSON.stringify({
    source: 'e2e-waf',
    severity: 'high',
    eventType: 'sql_injection',
    summary: `拦截到 SQL 注入尝试 ${suffix}`,
    detailsUrl: 'https://example.com/events/1',
  })
  await postSigned(request, '/api/v1/security-events', eventBody, `e2e-event-${suffix}`, 201)

  await page.goto('/defense')
  await expect(page.getByRole('heading', { level: 1, name: '攻击防御' })).toBeVisible()
  await page.getByLabel('严重度', { exact: true }).selectOption('high')
  const eventRow = page.getByRole('row').filter({ hasText: `拦截到 SQL 注入尝试 ${suffix}` })
  await expect(eventRow).toBeVisible({ timeout: 20_000 })
  await expect(eventRow.getByText('高', { exact: true })).toBeVisible()
  await expect(eventRow).toContainText('sql_injection')
  await expect(eventRow).toContainText('e2e-waf')
  await expect(eventRow.getByText('开放', { exact: true })).toBeVisible()

  // 二次确认后变为已确认并记录确认人。
  await eventRow.getByRole('button', { name: '确认', exact: true }).click()
  const ackDialog = page.getByRole('dialog')
  await expect(ackDialog.getByRole('heading', { name: '确认防御事件' })).toBeVisible()
  await ackDialog.getByRole('button', { name: '确认事件' }).click()
  await expect(page.getByText(/已确认事件 sql_injection/)).toBeVisible()
  const ackedRow = page.getByRole('row').filter({ hasText: `拦截到 SQL 注入尝试 ${suffix}` })
  // 状态列徽章与操作列文案都显示「已确认」，取第一个（状态徽章）。
  await expect(ackedRow.getByText('已确认', { exact: true }).first()).toBeVisible()
  await expect(ackedRow).toContainText('e2e-admin')
  await page.getByLabel('状态', { exact: true }).selectOption('acknowledged')
  await expect(page.getByRole('row').filter({ hasText: `拦截到 SQL 注入尝试 ${suffix}` })).toBeVisible()
  await page.getByLabel('状态', { exact: true }).selectOption('open')
  await expect(page.getByRole('row').filter({ hasText: `拦截到 SQL 注入尝试 ${suffix}` })).toHaveCount(0)

  // 重复确认幂等：直接调接口仍是 200 且状态保持已确认。
  const repeatAck = await page.evaluate(async (eventType) => {
    const { apiRequest } = await import('/src/stores/session.ts')
    const result = await apiRequest<{ id: string; status: string }>('/security-events?limit=100&offset=0')
    const target = result.items.find((item) => item.eventType === eventType) as { id: string } | undefined
    if (!target) return { status: 'missing' }
    const acked = await apiRequest<{ status: string }>(
      `/security-events/${encodeURIComponent(target.id)}/ack`,
      { method: 'POST', body: '{}' },
    )
    return { status: acked.status }
  }, 'sql_injection')
  expect(repeatAck.status).toBe('acknowledged')

  // ---- 非 admin：防御页被守卫拒绝、入口隐藏、接口 403；扫描读仍可用 ----
  const memberName = `defense-member-${suffix}`
  await createUser(page, memberName, 'member')
  const browser = page.context().browser()!
  const memberCtx = await browser.newContext()
  const member = await memberCtx.newPage()
  await login(member, memberName, 'Defense-pass-2026')

  await expect(member.locator('aside').getByRole('link', { name: '攻击防御' })).toHaveCount(0)
  await member.goto('/defense')
  await expect(member.getByRole('heading', { name: '无权访问' })).toBeVisible()
  const forbidden = await member.evaluate(async () => {
    const { apiRequest } = await import('/src/stores/session.ts')
    try {
      await apiRequest('/security-events?limit=20&offset=0')
      return { status: 200, message: '' }
    } catch (error) {
      const apiError = error as { status: number; message: string }
      return { status: apiError.status, message: apiError.message }
    }
  })
  expect(forbidden.status).toBe(403)
  // 403 文案来自统一错误码映射（中文），不泄露后端英文 message。
  expect(forbidden.message).toBe('当前账号没有执行该操作的权限')

  // 扫描读取对普通成员开放（项目 public），防御数据不可见。
  await member.goto('/scans')
  await expect(member.getByRole('heading', { level: 1, name: '安全扫描' })).toBeVisible()
  await member.getByLabel('项目', { exact: true }).selectOption(projectId)
  // 该项目有两条扫描（passed/failed），按唯一摘要定位避免 strict mode 歧义。
  await expect(member.getByRole('row').filter({ hasText: '发现 3 个高危漏洞' })).toBeVisible({
    timeout: 20_000,
  })
  await memberCtx.close()
})
