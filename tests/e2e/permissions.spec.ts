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

async function createMember(page: Page, username: string) {
  await page.evaluate(async ({ username }) => {
    const { apiRequest } = await import('/src/stores/session.ts')
    return apiRequest('/users', {
      method: 'POST',
      body: JSON.stringify({ username, password: 'Member-pass-2026', displayName: username, role: 'member' }),
    })
  }, { username })
}

test('项目可见性与成员：私有项目、邀请、非成员不可见、maintainer 才可写', async ({ page }) => {
  await login(page)
  const suffix = Date.now()
  const name = `e2e-private-${suffix}`
  // 两个普通 member：一个将被邀请为查看者，一个保持局外人。先建账号，
  // 成员页的用户下拉在挂载时读取一次目录。
  const viewerName = `perm-viewer-${suffix}`
  const outsiderName = `perm-outsider-${suffix}`
  await createMember(page, viewerName)
  await createMember(page, outsiderName)

  // 通过向导创建私有项目。
  await page.goto('/')
  await page.getByRole('link', { name: '创建项目' }).click()
  await page.getByLabel('项目名称').fill(name)
  await page.getByLabel('构建命令').fill('echo private-ok')
  await page.getByLabel('可见性').selectOption('private')
  await page.getByRole('button', { name: '创建项目', exact: true }).click()
  await expect(page).toHaveURL('/')
  const projectRow = page.getByRole('row', { name: new RegExp(name) })
  await expect(projectRow.getByText('私有')).toBeVisible()

  const projectID = await page.evaluate(async (projectName) => {
    const { apiRequest } = await import('/src/stores/session.ts')
    const result = await apiRequest<{ items: { id: string; name: string }[] }>('/projects?limit=100&offset=0')
    return result.items.find((item) => item.name === projectName)?.id ?? ''
  }, name)
  expect(projectID).not.toBe('')

  // 成员页：创建者自动是维护者，其他成员还没有。
  await projectRow.getByRole('link', { name: '成员' }).click()
  await expect(page).toHaveURL(new RegExp(`/projects/${projectID}/members$`))
  await expect(page.getByRole('heading', { name: new RegExp(`${name} · 成员`) })).toBeVisible()
  const creatorRow = page.getByRole('row', { name: /e2e-admin/ })
  await expect(creatorRow).toContainText('创建者')
  await expect(creatorRow).toContainText('维护者')

  // 两个普通 member 已在开头创建；这里只做邀请与验证。
  await page.getByLabel('用户', { exact: true }).selectOption({ label: viewerName })
  await page.getByLabel('角色', { exact: true }).selectOption('viewer')
  await page.getByRole('button', { name: '添加成员' }).click()
  const viewerRow = page.getByRole('row', { name: new RegExp(viewerName) })
  await expect(viewerRow).toContainText('查看者')

  const browser = page.context().browser()!

  // 非成员直接访问成员页 URL：得到「未找到」，不暴露项目存在。
  const outsiderCtx = await browser.newContext()
  const outsider = await outsiderCtx.newPage()
  await login(outsider, outsiderName, 'Member-pass-2026')
  await outsider.goto(`/projects/${projectID}/members`)
  await expect(outsider.getByText(/未找到该项目/)).toBeVisible()
  await outsiderCtx.close()

  // 查看者成员：能进成员页，但没有管理控件，且触发构建被服务端拒绝。
  const viewerCtx = await browser.newContext()
  const viewer = await viewerCtx.newPage()
  await login(viewer, viewerName, 'Member-pass-2026')
  await viewer.goto(`/projects/${projectID}/members`)
  await expect(viewer.getByRole('heading', { name: new RegExp(`${name} · 成员`) })).toBeVisible()
  await expect(viewer.getByRole('button', { name: '添加成员' })).toHaveCount(0)
  const forbidden = await viewer.evaluate(async (id) => {
    const { apiRequest } = await import('/src/stores/session.ts')
    try {
      await apiRequest(`/projects/${id}/builds`, { method: 'POST', body: '{}' })
      return 201
    } catch (error) {
      return (error as { status: number }).status
    }
  }, projectID)
  expect(forbidden).toBe(403)
  await viewerCtx.close()

  // 提升为维护者后，同一个账号可以触发构建。
  await viewerRow.getByRole('button', { name: '设为维护者' }).click()
  await expect(viewerRow).toContainText('维护者')

  const maintainerCtx = await browser.newContext()
  const maintainer = await maintainerCtx.newPage()
  await login(maintainer, viewerName, 'Member-pass-2026')
  await maintainer.goto('/')
  const row = maintainer.getByRole('row', { name: new RegExp(name) })
  await expect(row).toBeVisible()
  await row.getByRole('button', { name: '构建', exact: true }).click()
  await expect(maintainer).toHaveURL('/builds')
  await expect(maintainer.getByRole('row', { name: new RegExp(name) })).toBeVisible()
  await maintainerCtx.close()
})
