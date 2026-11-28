import { test, expect } from '@playwright/test'
import type { Page } from '@playwright/test'
import { randomBytes } from 'node:crypto'
import {
  createAdminInTenant,
  createTenantWithAdmin,
  listTenants,
  type CreatedAdmin,
  type CreatedTenant,
} from './tenantCli'

/**
 * 阶段 F3 双租户验收（真实后端 + PostgreSQL + Redis + 真实 `cmd/tenant` CLI）。
 *
 * 租户开通没有 HTTP API，所以第二个租户与管理员完全由运维 CLI 创建；本文件
 * 创建一个唯一 slug 的租户 B，在真实登录页用 tenantSlug 登录 B，并验证：
 * 1. B 的顶栏展示当前工作空间名称与 slug，B 内创建的私有项目在 A 侧完全不可见
 *    （列表、深链 404、API 404、审计与 CSV 隔离）；
 * 2. 同一登录标识存在于两个租户时，不带 tenantSlug 的登录返回 `tenant_required`，
 *    登录页展开「工作空间标识」并保留邮箱/密码，补填 B 的 slug 后登录成功；
 * 3. 单租户既有流程不受影响（其余 spec 全部通过，登录页默认不显示工作空间输入）。
 */
test.describe.configure({ timeout: 150_000 })

const admin = { username: 'e2e-admin', password: 'E2e-admin-password-2026' }
/** 每次运行唯一的租户 slug 与项目名，避免残留数据（E2E 库本应每次重置）。 */
const suffix = randomBytes(4).toString('hex')
const tenantB = { slug: `e2e-b-${suffix}`, name: `E2E 工作空间 B ${suffix}` }
const bProjectName = `B 私有项目 ${suffix}`

let createdTenant: CreatedTenant
let bAdmin: CreatedAdmin
let bProjectID = ''

test.beforeAll(async () => {
  const created = await createTenantWithAdmin({
    slug: tenantB.slug,
    name: tenantB.name,
    // CLI 用 email local part 派生用户名；随机后缀保证只属于本次运行。
    adminEmail: `b-admin-${suffix}@e2e.example`,
  })
  createdTenant = created.tenant
  bAdmin = created.admin
})

async function login(page: Page, username: string, password: string, tenantSlug?: string) {
  await page.goto(tenantSlug ? `/login?tenant=${encodeURIComponent(tenantSlug)}` : '/login')
  if (tenantSlug) {
    // URL ?tenant= 预填并展开工作空间标识，用户无需菜单操作。
    await expect(page.getByLabel('工作空间标识', { exact: true })).toHaveValue(tenantSlug)
  }
  await page.getByLabel('用户名', { exact: true }).fill(username)
  await page.getByLabel('密码', { exact: true }).fill(password)
  await page.getByRole('button', { name: '登录', exact: true }).click()
  await expect(page).toHaveURL('/')
}

/** 头像菜单 → 退出登录 → 确认（与既有鉴权用例相同的真实退出路径）。 */
async function logout(page: Page) {
  await page.getByRole('button', { name: '打开用户菜单' }).click()
  await page.getByRole('menuitem', { name: '退出登录' }).click()
  await page.getByRole('button', { name: '确认退出' }).click()
  await expect(page).toHaveURL(/\/login/)
}

/** 顶栏用户信息附近展示当前租户：名称可见，slug 在 title 提示里。 */
async function expectCurrentTenant(page: Page, tenant: { slug: string; name: string }) {
  const indicator = page.getByTestId('current-tenant')
  await expect(indicator).toBeVisible()
  await expect(indicator).toContainText(tenant.name)
  await expect(indicator).toHaveAttribute('title', tenant.slug)
}

/** 通过真实 UI 创建私有项目并返回项目 id（项目名唯一，断言限定在本次运行）。 */
async function createPrivateProject(page: Page, name: string): Promise<string> {
  await page.goto('/')
  await page.getByRole('link', { name: '创建项目' }).click()
  await page.getByLabel('项目名称').fill(name)
  await page.getByLabel('可见性').selectOption('private')
  await page.getByRole('button', { name: '创建项目', exact: true }).click()
  await expect(page).toHaveURL('/')
  const row = page.getByRole('row', { name: new RegExp(name) })
  await expect(row).toBeVisible()
  await expect(row.getByText('私有', { exact: true })).toBeVisible()
  const id = await page.evaluate(async (projectName) => {
    const { apiRequest } = await import('/src/stores/session.ts')
    const result = await apiRequest<{ items: { id: string; name: string }[] }>('/projects?limit=100&offset=0')
    return result.items.find((project) => project.name === projectName)?.id ?? ''
  }, name)
  expect(id).not.toBe('')
  return id
}

test('双租户隔离：B 工作空间登录、私有项目与 A 侧完全不可见', async ({ page }) => {
  // 1) 用 CLI 创建的 B 管理员经真实登录页（?tenant=<slug> 预填）登录 B。
  await login(page, bAdmin.username, bAdmin.password, tenantB.slug)
  await expectCurrentTenant(page, tenantB)
  // CLI list 是部署视角：两租户都在，B 的 id/slug/name 与创建结果一致。
  const tenants = await listTenants()
  const bRow = tenants.find((tenant) => tenant.slug === tenantB.slug)
  expect(bRow).toBeDefined()
  expect(bRow?.id).toBe(createdTenant.id)
  expect(bRow?.name).toBe(tenantB.name)
  expect(bRow?.status).toBe('active')
  expect(tenants.some((tenant) => tenant.slug !== tenantB.slug)).toBe(true)

  // 2) 在 B 内创建私有项目，A 侧不可见。
  bProjectID = await createPrivateProject(page, bProjectName)
  await logout(page)

  // 3) 默认租户 A 管理员登录：顶栏显示各自的工作空间，项目列表不含 B 项目。
  await login(page, admin.username, admin.password)
  await expect(page.getByTestId('current-tenant')).toContainText('Default')
  await expect(page.getByTestId('current-tenant')).not.toHaveAttribute('title', tenantB.slug)
  const projectRow = page.getByRole('row', { name: new RegExp(bProjectName) })
  await expect(projectRow).toHaveCount(0)
  const visibleProjects = await page.evaluate(async () => {
    const { apiRequest } = await import('/src/stores/session.ts')
    return apiRequest<{ items: { id: string; name: string }[] }>('/projects?limit=100&offset=0')
  })
  expect(visibleProjects.items.some((project) => project.id === bProjectID)).toBe(false)
  expect(visibleProjects.items.some((project) => project.name === bProjectName)).toBe(false)

  // 4) 深链与直接 API 都返回 404（不泄露存在性）。
  await page.goto(`/projects/${bProjectID}/edit`)
  await expect(page.getByText(/未找到该项目/)).toBeVisible()
  const directStatus = await page.evaluate(async (projectID) => {
    const { apiRequest } = await import('/src/stores/session.ts')
    try {
      await apiRequest(`/projects/${projectID}`)
      return 200
    } catch (error) {
      return (error as { status: number }).status
    }
  }, bProjectID)
  expect(directStatus).toBe(404)

  // 5) 审计页与 API 都没有 B 租户的动作：B 管理员的创建项目审计只属于 B。
  const bAdminAudit = await page.evaluate(async (actor) => {
    const { apiRequest } = await import('/src/stores/session.ts')
    return apiRequest<{ items: unknown[]; total: number }>(`/audit?actor=${encodeURIComponent(actor)}&limit=100&offset=0`)
  }, bAdmin.username)
  expect(bAdminAudit.total).toBe(0)
  expect(bAdminAudit.items).toHaveLength(0)
  await page.goto('/settings')
  await page.getByRole('button', { name: '审计', exact: true }).click()
  const auditTable = page.getByRole('table')
  await expect(auditTable.getByText(bAdmin.username)).toHaveCount(0)
  await expect(auditTable.getByText(createdTenant.id)).toHaveCount(0)
  const fullAudit = await page.evaluate(async () => {
    const { apiRequest } = await import('/src/stores/session.ts')
    return apiRequest<{ items: { action: string; objectId: string; actorUsername: string }[] }>('/audit?limit=100&offset=0')
  })
  expect(fullAudit.items.some((entry) => entry.objectId === bProjectID)).toBe(false)
  await logout(page)
})

test('跨租户同标识登录：tenant_required 提示、保留输入、补填工作空间标识后登录 B', async ({ page }) => {
  // 同一个登录标识（用户名）同时存在于 A 与 B：A 侧用真实用户接口创建一个
  // admin，B 侧用 CLI create-admin 的「email local part 派生用户名」创建同名
  // admin。bootstrap 管理员没有 email，无法与 CLI 的 email 参数对上，因此用
  // A 侧新建的同名 admin 复核跨租户歧义（后端登录查找按 username）。
  const duplicateName = `dual-${suffix}`
  const dualPassword = 'Dual-admin-password-2026'

  await login(page, admin.username, admin.password)
  await page.evaluate(async ({ username, password }) => {
    const { apiRequest } = await import('/src/stores/session.ts')
    await apiRequest('/users', {
      method: 'POST',
      body: JSON.stringify({ username, password, displayName: 'Dual Workspace Admin', role: 'admin' }),
    })
  }, { username: duplicateName, password: dualPassword })
  await logout(page)

  const duplicate = await createAdminInTenant(tenantB.slug, `${duplicateName}@e2e.example`)
  expect(duplicate.username).toBe(duplicateName)

  // 单租户体验不变：默认登录页不显示工作空间标识输入。
  await page.goto('/login')
  await expect(page.getByLabel('工作空间标识', { exact: true })).toHaveCount(0)
  await page.getByLabel('用户名', { exact: true }).fill(duplicate.username)
  await page.getByLabel('密码', { exact: true }).fill(duplicate.password)
  await page.getByRole('button', { name: '登录', exact: true }).click()

  // 400 tenant_required：中文提示出现，工作空间标识输入展开，邮箱与密码保留。
  const alert = page.getByRole('alert')
  await expect(alert).toContainText('该邮箱存在于多个工作空间，请输入工作空间标识')
  const tenantField = page.getByLabel('工作空间标识', { exact: true })
  await expect(tenantField).toBeVisible()
  await expect(page.getByLabel('用户名', { exact: true })).toHaveValue(duplicate.username)
  await expect(page.getByLabel('密码', { exact: true })).toHaveValue(duplicate.password)

  // 客户端格式校验（全小写/数字/中间短横线）先于请求给出中文错误。
  await tenantField.fill('Not-A-Slug')
  await page.getByRole('button', { name: '登录', exact: true }).click()
  await expect(page.getByRole('alert')).toContainText('只能包含小写字母、数字和短横线')
  await expect(page).toHaveURL(/\/login/)

  // 补填 B 的 slug 后登录成功：顶栏切换为 B，错误态清空。
  await tenantField.fill(tenantB.slug)
  await page.getByRole('button', { name: '登录', exact: true }).click()
  await expect(page).toHaveURL('/')
  await expectCurrentTenant(page, tenantB)
  await expect(page.getByRole('alert')).toHaveCount(0)
  await logout(page)
})
