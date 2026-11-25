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

const UNIMPLEMENTED_TEXT = '此模块的真实业务接口尚未实现，当前不提供演示数据或操作按钮。'

test('未实现模块恒定显示声明，不提供演示数据或伪造操作', async ({ page }) => {
  await login(page)

  // 侧边栏入口真实存在：从项目列表可导航到审查中心。
  await page.locator('aside').getByRole('link', { name: '审查中心' }).click()
  await expect(page).toHaveURL('/reviews')

  for (const [path, title] of [
    ['/reviews', '审查中心'],
    ['/scans', '安全扫描'],
  ] as const) {
    await page.goto(path)
    await expect(page).toHaveURL(path)
    const section = page.locator('section[aria-labelledby="unimplemented-title"]')
    await expect(section).toBeVisible()
    await expect(page.getByRole('heading', { level: 1, name: title })).toBeVisible()
    await expect(section.getByText(UNIMPLEMENTED_TEXT)).toBeVisible()
    // 声明页不得出现演示数据或伪造操作：没有按钮、表单控件、表格或进度条。
    await expect(
      section.locator('button, input, select, textarea, table, [role="button"], [role="progressbar"]'),
    ).toHaveCount(0)
    // 刷新后声明仍然恒定，而不是一次性的演示开关。
    await page.reload()
    await expect(page.getByRole('heading', { level: 1, name: title })).toBeVisible()
    await expect(section.getByText(/此模块的真实业务接口尚未实现/)).toBeVisible()
  }

  // 详情路由同样恒定未实现，不会退化成带演示数据的详情页。
  await page.goto('/reviews/example-review-id')
  await expect(page.getByRole('heading', { level: 1, name: '审查详情' })).toBeVisible()
  await expect(page.getByText(/此模块的真实业务接口尚未实现/)).toBeVisible()
})
