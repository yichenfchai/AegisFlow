import { test, expect } from '@playwright/test'
import type { Page } from '@playwright/test'
import { createServer, type Server } from 'node:http'
import type { AddressInfo } from 'node:net'

const admin = { username: 'e2e-admin', password: 'E2e-admin-password-2026' }

let upstream: Server
let upstreamPort = 0

test.beforeAll(async () => {
  // 部署门禁用例需要一个返回 2xx 的部署端点，用本 spec 内的 Node stub 代替真实上游。
  upstream = createServer((req, res) => {
    req.resume()
    req.on('end', () => {
      res.writeHead(200, { 'Content-Type': 'text/plain' })
      res.end('review deploy ok\n')
    })
  })
  await new Promise<void>((resolve) => upstream.listen(0, '127.0.0.1', resolve))
  upstreamPort = (upstream.address() as AddressInfo).port
})

test.afterAll(async () => {
  await new Promise<void>((resolve) => {
    upstream.close(() => resolve())
    // Go 的 HTTP client 会保留 keep-alive 连接，主动断开避免 Playwright 进程等待。
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

/** 通过真实接口准备数据，保持 UI 用例聚焦在被测交互上。 */
async function createProject(
  page: Page,
  name: string,
  visibility: 'public' | 'private' = 'public',
): Promise<string> {
  return page.evaluate(
    async ({ name, visibility }) => {
      const { apiRequest } = await import('/src/stores/session.ts')
      const project = await apiRequest<{ id: string }>('/projects', {
        method: 'POST',
        body: JSON.stringify({ name, buildCommand: 'echo review-e2e', visibility }),
      })
      return project.id
    },
    { name, visibility },
  )
}

async function createReview(page: Page, projectId: string, subject: string, version = 'v1.0.0'): Promise<string> {
  return page.evaluate(
    async ({ projectId, subject, version }) => {
      const { apiRequest } = await import('/src/stores/session.ts')
      const review = await apiRequest<{ id: string }>(
        `/projects/${encodeURIComponent(projectId)}/reviews`,
        { method: 'POST', body: JSON.stringify({ subject, version }) },
      )
      return review.id
    },
    { projectId, subject, version },
  )
}

async function approveReview(page: Page, reviewId: string, reason: string) {
  await page.evaluate(
    async ({ reviewId, reason }) => {
      const { apiRequest } = await import('/src/stores/session.ts')
      await apiRequest(`/reviews/${encodeURIComponent(reviewId)}/approve`, {
        method: 'POST',
        body: JSON.stringify({ reason }),
      })
    },
    { reviewId, reason },
  )
}

async function createUser(page: Page, username: string, role: 'member' | 'viewer') {
  await page.evaluate(
    async ({ username, role }) => {
      const { apiRequest } = await import('/src/stores/session.ts')
      await apiRequest('/users', {
        method: 'POST',
        body: JSON.stringify({ username, password: 'Review-pass-2026', displayName: username, role }),
      })
    },
    { username, role },
  )
}

async function createTarget(page: Page, name: string): Promise<string> {
  return page.evaluate(
    async ({ name, port }) => {
      const { apiRequest } = await import('/src/stores/session.ts')
      const target = await apiRequest<{ id: string }>('/deployment-targets', {
        method: 'POST',
        body: JSON.stringify({ name, environment: 'staging', endpoint: `http://127.0.0.1:${port}` }),
      })
      return target.id
    },
    { name, port: upstreamPort },
  )
}

test('审查中心：发起、批准（带理由）、状态筛选与拒绝理由必填', async ({ page }) => {
  test.setTimeout(120_000)
  await login(page)
  const suffix = Date.now()
  const projectName = `review-proj-${suffix}`

  // 侧栏入口进入真实的审查中心。
  await page.locator('aside').getByRole('link', { name: '审查中心' }).click()
  await expect(page).toHaveURL('/reviews')
  await expect(page.getByRole('heading', { level: 1, name: '审查中心' })).toBeVisible()

  // 先通过 UI 创建一个项目（列表与对话框的项目来源）。
  await page.goto('/')
  await page.getByRole('link', { name: '创建项目' }).click()
  await page.getByLabel('项目名称').fill(projectName)
  await page.getByRole('button', { name: '创建项目', exact: true }).click()
  await expect(page).toHaveURL('/')
  await expect(page.getByRole('row', { name: new RegExp(projectName) })).toBeVisible()
  const projectId = await page.evaluate(async (name) => {
    const { apiRequest } = await import('/src/stores/session.ts')
    const result = await apiRequest<{ items: { id: string; name: string }[] }>('/projects?limit=100&offset=0')
    return result.items.find((item) => item.name === name)?.id ?? ''
  }, projectName)
  expect(projectId).not.toBe('')

  // 列表页对话框发起第一条审查。
  const subject = `发布 v1.0.0 ${suffix}`
  await page.goto('/reviews')
  await page.getByRole('button', { name: '发起审查', exact: true }).click()
  const createDialog = page.getByRole('dialog')
  await expect(createDialog.getByRole('heading', { name: '发起审查' })).toBeVisible()
  await expect(createDialog.getByLabel('项目', { exact: true })).toContainText(projectName, { timeout: 20_000 })
  await createDialog.getByLabel('项目', { exact: true }).selectOption(projectId)
  await createDialog.getByLabel('审查主题').fill(subject)
  await createDialog.getByLabel('描述').fill('验证审查中心主链路')
  await createDialog.getByLabel('版本').fill('v1.0.0')
  await createDialog.getByLabel('外部链接').fill('https://example.com/reviews/1?state=open')
  await createDialog.getByRole('button', { name: '发起审查', exact: true }).click()
  await expect(page).toHaveURL(/\/reviews\/[A-Za-z0-9_-]{16,64}$/)
  const firstReviewUrl = page.url()

  // 详情：待审查，外部链接新标签打开。
  await expect(page.getByRole('heading', { level: 1, name: /审查 #1$/ })).toBeVisible()
  await expect(page.getByText('待审查', { exact: true }).first()).toBeVisible()
  await expect(page.getByText(subject, { exact: true }).first()).toBeVisible()
  const externalLink = page.getByRole('link', { name: '在外部系统查看' })
  await expect(externalLink).toHaveAttribute('target', '_blank')
  await expect(externalLink).toHaveAttribute('rel', /noopener/)
  await expect(externalLink).toHaveAttribute('href', 'https://example.com/reviews/1?state=open')

  // 批准（可选理由）。
  const approveReason = `变更范围已确认 ${suffix}`
  await page.getByRole('button', { name: '批准', exact: true }).click()
  const approveDialog = page.getByRole('dialog')
  await approveDialog.getByLabel('批准理由').fill(approveReason)
  await approveDialog.getByRole('button', { name: '确认批准' }).click()
  await expect(page.getByText('已批准该审查', { exact: true })).toBeVisible()
  await expect(page.getByText('已批准', { exact: true }).first()).toBeVisible()
  await expect(page.getByText(approveReason, { exact: true })).toBeVisible()
  await expect(page.getByRole('button', { name: '批准', exact: true })).toHaveCount(0)

  // 列表按状态筛选能命中已批准记录。
  await page.goto('/reviews')
  await page.getByLabel('状态', { exact: true }).selectOption('approved')
  const approvedRow = page.getByRole('row').filter({ hasText: subject })
  await expect(approvedRow).toBeVisible()
  await expect(approvedRow.getByText('已批准')).toBeVisible()
  await page.getByLabel('状态', { exact: true }).selectOption('pending')
  await expect(page.getByRole('row').filter({ hasText: subject })).toHaveCount(0)
  await page.getByLabel('项目', { exact: true }).selectOption(projectId)
  await page.getByLabel('状态', { exact: true }).selectOption('approved')
  await expect(page.getByRole('row').filter({ hasText: subject })).toBeVisible()

  // 第二条审查：拒绝理由必填。
  const rejectSubject = `需要补充回滚方案 ${suffix}`
  await page.getByRole('button', { name: '发起审查', exact: true }).click()
  const secondDialog = page.getByRole('dialog')
  await expect(secondDialog.getByLabel('项目', { exact: true })).toContainText(projectName, { timeout: 20_000 })
  await secondDialog.getByLabel('项目', { exact: true }).selectOption(projectId)
  await secondDialog.getByLabel('审查主题').fill(rejectSubject)
  await secondDialog.getByRole('button', { name: '发起审查', exact: true }).click()
  await expect(page).toHaveURL(/\/reviews\/[A-Za-z0-9_-]{16,64}$/)
  await expect(page.getByText('待审查', { exact: true }).first()).toBeVisible()
  expect(page.url()).not.toBe(firstReviewUrl)

  await page.getByRole('button', { name: '拒绝', exact: true }).click()
  const rejectDialog = page.getByRole('dialog')
  await expect(rejectDialog.getByRole('heading', { name: /拒绝审查/ })).toBeVisible()
  // 未填理由时前端拦截，对话框保持打开且不发出请求。
  await rejectDialog.getByRole('button', { name: '确认拒绝' }).click()
  await expect(rejectDialog.getByText('请填写拒绝理由')).toBeVisible()
  await expect(rejectDialog).toBeVisible()
  const rejectReason = `缺少回滚方案 ${suffix}`
  await rejectDialog.getByLabel('拒绝理由').fill(rejectReason)
  await rejectDialog.getByRole('button', { name: '确认拒绝' }).click()
  await expect(page.getByText('已拒绝该审查', { exact: true })).toBeVisible()
  await expect(page.getByText('已拒绝', { exact: true }).first()).toBeVisible()
  await expect(page.getByText(rejectReason, { exact: true })).toBeVisible()

  // 拒绝后的记录在列表 rejected 筛选中可见并带理由。
  await page.goto('/reviews')
  await page.getByLabel('状态', { exact: true }).selectOption('rejected')
  const rejectedRow = page.getByRole('row').filter({ hasText: rejectSubject })
  await expect(rejectedRow).toBeVisible()
  await expect(rejectedRow.getByText(rejectReason)).toBeVisible()
})

test('部署表单：可选已批准审查创建部署，详情显示审查链接', async ({ page }) => {
  test.setTimeout(120_000)
  await login(page)
  const suffix = Date.now()
  const projectId = await createProject(page, `review-deploy-${suffix}`)
  const reviewId = await createReview(page, projectId, `部署门禁审查 ${suffix}`, 'v2.0.0')
  await approveReview(page, reviewId, '部署前确认')
  const targetId = await createTarget(page, `review-target-${suffix}`)

  await page.goto('/deploys/new')
  await page.getByLabel('项目').selectOption(projectId)
  // 下拉只包含该项目 status=approved 的审查。
  await expect(page.getByLabel('关联网关审查')).toContainText('#1', { timeout: 20_000 })
  await expect(page.getByLabel('关联网关审查')).toContainText('部署门禁审查')
  await page.getByLabel('关联网关审查').selectOption(reviewId)
  await expect(page.getByLabel('目标')).toContainText(`review-target-${suffix}`, { timeout: 20_000 })
  await page.getByLabel('目标').selectOption(targetId)
  await page.getByLabel('版本').fill('v2.0.0')
  await page.getByRole('button', { name: '创建部署', exact: true }).click()

  await expect(page).toHaveURL(/\/deploys\/[A-Za-z0-9_-]{16,64}$/)
  await expect(page.getByText('成功', { exact: true }).first()).toBeVisible({ timeout: 20_000 })
  const reviewLink = page.getByRole('link', { name: '查看关联审查' })
  await expect(reviewLink).toBeVisible()
  await expect(reviewLink).toHaveAttribute('href', `/reviews/${reviewId}`)
  await reviewLink.click()
  await expect(page).toHaveURL(`/reviews/${reviewId}`)
  await expect(page.getByRole('heading', { level: 1, name: /审查 #1$/ })).toBeVisible()
})

test('审查中心权限：viewer 无发起入口且发起 403，非项目成员详情 404', async ({ page }) => {
  test.setTimeout(120_000)
  await login(page)
  const suffix = Date.now()
  const viewSubject = `只读可见的审查 ${suffix}`
  const projectId = await createProject(page, `review-visible-${suffix}`)
  const reviewId = await createReview(page, projectId, viewSubject)

  // viewer：能看审查列表与详情，但没有发起/决策入口；直接调接口 403。
  const viewerName = `review-viewer-${suffix}`
  await createUser(page, viewerName, 'viewer')
  const browser = page.context().browser()!
  const viewerCtx = await browser.newContext()
  const viewer = await viewerCtx.newPage()
  await login(viewer, viewerName, 'Review-pass-2026')
  await viewer.goto('/reviews')
  await expect(viewer.getByRole('heading', { level: 1, name: '审查中心' })).toBeVisible()
  await expect(viewer.getByRole('row').filter({ hasText: viewSubject })).toBeVisible()
  await expect(viewer.getByRole('button', { name: '发起审查', exact: true })).toHaveCount(0)
  await viewer.goto(`/reviews/${reviewId}`)
  await expect(viewer.getByRole('heading', { level: 1, name: /审查 #1$/ })).toBeVisible()
  await expect(viewer.getByRole('button', { name: '批准', exact: true })).toHaveCount(0)
  await expect(viewer.getByRole('button', { name: '拒绝', exact: true })).toHaveCount(0)
  await expect(viewer.getByRole('button', { name: '取消审查' })).toHaveCount(0)
  const forbidden = await viewer.evaluate(async (id) => {
    const { apiRequest } = await import('/src/stores/session.ts')
    try {
      await apiRequest(`/projects/${encodeURIComponent(id)}/reviews`, {
        method: 'POST',
        body: JSON.stringify({ subject: 'viewer 不应成功' }),
      })
      return { status: 201, message: '' }
    } catch (error) {
      const apiError = error as { status: number; message: string }
      return { status: apiError.status, message: apiError.message }
    }
  }, projectId)
  expect(forbidden.status).toBe(403)
  // 403 文案来自统一错误码映射（中文），不泄露后端英文 message。
  expect(forbidden.message).toBe('当前账号没有执行该操作的权限')
  await viewerCtx.close()

  // 非项目成员：私有项目的审查详情按不可见处理（404），页面不泄露项目存在。
  const privateProjectId = await createProject(page, `review-private-${suffix}`, 'private')
  const privateReviewId = await createReview(page, privateProjectId, `私有审查 ${suffix}`)
  const outsiderName = `review-outsider-${suffix}`
  await createUser(page, outsiderName, 'member')
  const outsiderCtx = await browser.newContext()
  const outsider = await outsiderCtx.newPage()
  await login(outsider, outsiderName, 'Review-pass-2026')
  const notFound = await outsider.evaluate(async (id) => {
    const { apiRequest } = await import('/src/stores/session.ts')
    try {
      await apiRequest(`/reviews/${encodeURIComponent(id)}`)
      return { status: 200 }
    } catch (error) {
      return { status: (error as { status: number }).status }
    }
  }, privateReviewId)
  expect(notFound.status).toBe(404)
  await outsider.goto(`/reviews/${privateReviewId}`)
  await expect(outsider.getByText(/未找到该审查/)).toBeVisible()
  await outsiderCtx.close()
})
