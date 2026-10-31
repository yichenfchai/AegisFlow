import { test, expect } from '@playwright/test'
import type { Page } from '@playwright/test'

const admin = { username: 'e2e-admin', password: 'E2e-admin-password-2026' }

async function login(page: Page) {
  await page.goto('/login')
  await page.getByLabel('用户名', { exact: true }).fill(admin.username)
  await page.getByLabel('密码', { exact: true }).fill(admin.password)
  await page.getByRole('checkbox').setChecked(true)
  await page.getByRole('button', { name: '登录', exact: true }).click()
  await expect(page).toHaveURL('/')
}

test('审计筛选：动作与操作人叠加、日期范围与重置', async ({ page }) => {
  await login(page)

  // 通过真实 UI 创建成员；该操作写入一条操作人为 e2e-admin 的 user.create 审计。
  const memberName = `audit-member-${Date.now()}`
  await page.goto('/settings')
  await page.getByRole('button', { name: '创建用户' }).click()
  await page.getByLabel('用户名').fill(memberName)
  await page.getByLabel('显示名').fill('审计筛选')
  await page.getByLabel('初始密码').fill('Audit-member-password-2026')
  await page.getByLabel('角色').selectOption('member')
  await page.getByRole('button', { name: '创建', exact: true }).click()
  await expect(page.getByRole('row', { name: new RegExp(memberName) })).toBeVisible()

  // 切到审计页：未筛选时表格有行。
  await page.getByRole('button', { name: '审计', exact: true }).click()
  const actionCells = page.locator('tbody tr td:nth-child(3)')
  const actorCells = page.locator('tbody tr td:nth-child(2)')
  await expect(page.locator('tbody tr').first()).toBeVisible()

  // 动作筛选：只剩「创建用户」（user.create）。
  await page.getByLabel('动作', { exact: true }).selectOption({ label: '创建用户' })
  await expect
    .poll(async () => {
      const texts = await actionCells.allTextContents()
      return texts.length > 0 && texts.every((text) => text === '创建用户')
    })
    .toBe(true)

  // 继续叠加操作人筛选：操作人只能是 e2e-admin。
  await page.getByLabel('操作人', { exact: true }).fill(admin.username)
  await expect
    .poll(async () => {
      const texts = await actorCells.allTextContents()
      return texts.length > 0 && texts.every((text) => text === admin.username)
    })
    .toBe(true)

  // 未来日期：没有符合条件的记录。
  await page.getByLabel('从', { exact: true }).fill('2999-01-01')
  await expect(page.getByText('没有符合筛选条件的记录')).toBeVisible()

  // 重置：筛选控件全部清空，数据行恢复。
  await page.getByRole('button', { name: '重置筛选' }).click()
  await expect(page.getByLabel('动作', { exact: true })).toHaveValue('')
  await expect(page.getByLabel('对象', { exact: true })).toHaveValue('')
  await expect(page.getByLabel('结果', { exact: true })).toHaveValue('')
  await expect(page.getByLabel('操作人', { exact: true })).toHaveValue('')
  await expect(page.getByLabel('从', { exact: true })).toHaveValue('')
  await expect(page.getByLabel('到', { exact: true })).toHaveValue('')
  await expect(page.getByText('没有符合筛选条件的记录')).toHaveCount(0)
  await expect(page.locator('tbody tr').first()).toBeVisible()
})
