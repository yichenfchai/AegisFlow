import { test, expect } from '@playwright/test'
import type { APIRequestContext, Page } from '@playwright/test'
import { E2E_REVIEW_WEBHOOK_SECRET, signExternalResult } from './reviewExternalResults'

const admin = { username: 'e2e-admin', password: 'E2e-admin-password-2026' }
// 机器接口不走浏览器会话：直接对后端签名 POST，避免经过 Vite 代理。
const backendURL = 'http://127.0.0.1:8080'

async function login(page: Page) {
  await page.goto('/login')
  await page.getByLabel('用户名', { exact: true }).fill(admin.username)
  await page.getByLabel('密码', { exact: true }).fill(admin.password)
  await page.getByRole('checkbox').setChecked(true)
  await page.getByRole('button', { name: '登录', exact: true }).click()
  await expect(page).toHaveURL('/')
}

async function createProject(page: Page, name: string): Promise<string> {
  return page.evaluate(async (name) => {
    const { apiRequest } = await import('/src/stores/session.ts')
    const project = await apiRequest<{ id: string }>('/projects', {
      method: 'POST',
      body: JSON.stringify({ name, buildCommand: 'echo external-results-e2e', visibility: 'public' }),
    })
    return project.id
  }, name)
}

interface ExternalResultInput {
  source: string
  verdict: 'passed' | 'failed' | 'warning'
  summary?: string
  url?: string
}

/**
 * 以外部工具身份回写一条结果：Node `crypto` 计算 HMAC-SHA256 签名，Playwright
 * `request` 直接 POST 后端机器接口，不依赖任何真实外部服务。
 */
async function postExternalResult(
  request: APIRequestContext,
  reviewId: string,
  input: ExternalResultInput,
  delivery: string,
  expectedStatus: number,
) {
  const rawBody = JSON.stringify(input)
  const timestamp = String(Math.floor(Date.now() / 1000))
  const response = await request.post(`${backendURL}/api/v1/reviews/${reviewId}/external-results`, {
    headers: {
      'Content-Type': 'application/json',
      'X-AegisFlow-Timestamp': timestamp,
      'X-AegisFlow-Signature': signExternalResult(E2E_REVIEW_WEBHOOK_SECRET, timestamp, rawBody),
      'X-AegisFlow-Delivery': delivery,
    },
    data: rawBody,
  })
  expect(response.status(), await response.text()).toBe(expectedStatus)
}

test('审查详情：外部结果签名回写后回显来源/徽章/摘要/外链，重复 delivery 幂等', async ({
  page,
  request,
}) => {
  test.setTimeout(120_000)
  await login(page)
  const suffix = Date.now()
  const projectName = `review-external-${suffix}`
  const projectId = await createProject(page, projectName)

  // 通过 UI 发起一条审查并进入详情，从详情 URL 捕获 review id。
  await page.goto('/reviews')
  await page.getByRole('button', { name: '发起审查', exact: true }).click()
  const dialog = page.getByRole('dialog')
  await expect(dialog.getByLabel('项目', { exact: true })).toContainText(projectName, {
    timeout: 20_000,
  })
  await dialog.getByLabel('项目', { exact: true }).selectOption(projectId)
  await dialog.getByLabel('审查主题').fill(`外部结果回写审查 ${suffix}`)
  await dialog.getByRole('button', { name: '发起审查', exact: true }).click()
  await expect(page).toHaveURL(/\/reviews\/[A-Za-z0-9_-]{16,64}$/)
  const reviewId = page.url().split('/').pop() ?? ''
  expect(reviewId).not.toBe('')

  // 尚无回写时显示空状态。
  const external = page.getByRole('region', { name: '外部结果' })
  await expect(external).toBeVisible()
  await expect(external.getByText('暂无外部结果', { exact: true })).toBeVisible()

  const passedSummary = '扫描通过：无高危漏洞\n共 0 个问题'
  const passedInput: ExternalResultInput = {
    source: 'e2e-scanner',
    verdict: 'passed',
    summary: passedSummary,
    url: 'https://example.com/report/1',
  }
  const passedDelivery = `e2e-passed-${suffix}`
  await postExternalResult(request, reviewId, passedInput, passedDelivery, 201)

  // 重新加载详情：来源、中文「通过」徽章、保留换行的摘要与外链。
  await page.reload()
  await expect(page.getByRole('heading', { level: 1, name: /审查 #1$/ })).toBeVisible()
  await expect(external.getByText('通过', { exact: true })).toBeVisible()
  await expect(external.getByText('e2e-scanner', { exact: true })).toBeVisible()
  await expect(external.getByText(/扫描通过：无高危漏洞/)).toContainText('共 0 个问题')
  await expect(external.getByText('刚刚').first()).toBeVisible()
  const reportLink = external.getByRole('link', { name: '查看外部报告' })
  await expect(reportLink).toHaveAttribute('href', 'https://example.com/report/1')
  await expect(reportLink).toHaveAttribute('target', '_blank')
  await expect(reportLink).toHaveAttribute('rel', /noopener noreferrer/)

  // 第二条不同 verdict：出现中文「警告」徽章与摘要。同一秒内到达的条目顺序
  // 由后端按 received_at 排序并以 id 稳定，前端不承诺同秒的展示顺序，因此这里
  // 不依赖「哪一条在第一位」。
  await postExternalResult(
    request,
    reviewId,
    { source: 'e2e-scanner', verdict: 'warning', summary: '覆盖率下降 5%' },
    `e2e-warning-${suffix}`,
    201,
  )
  await page.reload()
  await expect(external.getByText('警告', { exact: true })).toBeVisible()
  await expect(external.getByText('通过', { exact: true })).toHaveCount(1)
  await expect(external.getByText('e2e-scanner', { exact: true })).toHaveCount(2)
  const warningItem = external.locator('li').filter({ hasText: '警告' })
  await expect(warningItem).toHaveCount(1)
  await expect(warningItem).toContainText('覆盖率下降 5%')

  // 重复同一 delivery 再 POST：后端返回 200 + 既有记录，页面仍只显示一条。
  await postExternalResult(request, reviewId, passedInput, passedDelivery, 200)
  await page.reload()
  await expect(external.getByText('通过', { exact: true })).toHaveCount(1)
  await expect(external.getByText(/扫描通过：无高危漏洞/)).toHaveCount(1)
  await expect(external.getByText('e2e-scanner', { exact: true })).toHaveCount(2)
})
