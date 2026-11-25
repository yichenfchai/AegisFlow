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

test('项目编辑：名称/构建命令/分支/可见性保存生效，归档后只读', async ({ page }) => {
  await login(page)
  // 唯一项目名，避免上一轮 E2E 残留。
  const suffix = Date.now()
  const name = `e2e-edit-${suffix}`
  const renamed = `e2e-edit-${suffix}-renamed`

  // 创建：默认分支改为自由输入后可以直接填非 main/master 的引用名。
  await page.goto('/')
  await page.getByRole('link', { name: '创建项目' }).click()
  await page.getByLabel('项目名称').fill(name)
  await page.getByLabel('描述').fill('编辑前描述')
  await page.getByLabel('默认分支').fill('release/2026')
  await page.getByLabel('构建命令').fill('echo before-edit')
  await page.getByRole('button', { name: '创建项目', exact: true }).click()
  await expect(page).toHaveURL('/')
  await expect(page.getByRole('row', { name: new RegExp(name) })).toBeVisible()

  // 列表行提供编辑入口；编辑页加载服务端当前值。
  const projectRow = page.getByRole('row', { name: new RegExp(name) })
  await projectRow.getByRole('link', { name: '编辑' }).click()
  await expect(page).toHaveURL(/\/projects\/[A-Za-z0-9_-]{16,64}\/edit$/)
  await expect(page.getByLabel('项目名称')).toHaveValue(name)
  await expect(page.getByLabel('默认分支')).toHaveValue('release/2026')
  await expect(page.getByLabel('构建命令')).toHaveValue('echo before-edit')

  // 修改名称、构建命令与可见性后保存：回到列表并给出成功提示。
  await page.getByLabel('项目名称').fill(renamed)
  await page.getByLabel('构建命令').fill('echo after-edit')
  await page.getByLabel('可见性').selectOption('private')
  await page.getByRole('button', { name: '保存修改' }).click()
  await expect(page).toHaveURL('/')
  await expect(page.getByText(`项目「${renamed}」已更新`)).toBeVisible()
  const updatedRow = page.getByRole('row', { name: new RegExp(renamed) })
  await expect(updatedRow.getByText('私有')).toBeVisible()

  // 重新进入编辑页：修改已持久化（详情生效）。
  await updatedRow.getByRole('link', { name: '编辑' }).click()
  await expect(page.getByLabel('项目名称')).toHaveValue(renamed)
  await expect(page.getByLabel('构建命令')).toHaveValue('echo after-edit')
  await expect(page.getByLabel('可见性')).toHaveValue('private')
  await expect(page.getByLabel('默认分支')).toHaveValue('release/2026')

  // 接口侧同样读到新值，且列表排序按新名称命中。
  const persisted = await page.evaluate(async (projectName) => {
    const { apiRequest } = await import('/src/stores/session.ts')
    const result = await apiRequest<{ items: { id: string; name: string; buildCommand: string; visibility: string; defaultBranch: string }[] }>(
      '/projects?limit=100&offset=0',
    )
    return result.items.find((item) => item.name === projectName) ?? null
  }, renamed)
  expect(persisted).not.toBeNull()
  expect(persisted?.buildCommand).toBe('echo after-edit')
  expect(persisted?.visibility).toBe('private')
  expect(persisted?.defaultBranch).toBe('release/2026')

  // 归档后编辑页只读：有提示且不能保存。
  await page.goto('/')
  const activeRow = page.getByRole('row', { name: new RegExp(renamed) })
  await activeRow.getByRole('button', { name: '归档', exact: true }).click()
  await page.getByRole('button', { name: '确认归档' }).click()
  await expect(activeRow.getByText('已归档')).toBeVisible()
  await activeRow.getByRole('link', { name: '编辑' }).click()
  await expect(page.getByText(/项目已归档，当前为只读状态/)).toBeVisible()
  await expect(page.getByRole('button', { name: '保存修改' })).toBeDisabled()
  await expect(page.getByLabel('项目名称')).toBeDisabled()
})
