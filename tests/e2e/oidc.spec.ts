import { test, expect } from '@playwright/test'

/**
 * 可选 E2E：真实浏览器 + 本地假 OIDC provider（tests/e2e/fakeOidcProvider.mjs）。
 *
 * 仅当 AEGIS_E2E_OIDC=1 时 playwright.config.ts 才会启动假 provider 并给后端
 * 注入 AEGIS_OIDC_* 环境变量；否则本文件被 testIgnore 排除，默认 E2E 套件仍是
 * 未配置 OIDC 的纯密码场景（登录页不出现 SSO 按钮）。
 *
 * 覆盖主链路：登录页显示 SSO 按钮 → 跳转 authorize → 带 code+state 回到
 * callback（PKCE S256 由假 provider 校验）→ 自动开通 member 账号 → 跳回站内
 * returnTo → 会话可恢复、角色为 member（非 admin）。
 */
test.skip(!process.env.AEGIS_E2E_OIDC, 'set AEGIS_E2E_OIDC=1 to run the fake-provider SSO suite')

test('OIDC 单点登录：授权码+PKCE 登录并自动开通 member', async ({ page, context }) => {
  await page.goto('/login?returnTo=%2Fbuilds')
  const ssoButton = page.getByRole('button', { name: /企业单点登录/ })
  await expect(ssoButton).toBeVisible()
  await ssoButton.click()

  // authorize 自动带 code 302 回 callback；后端签发会话后回到 returnTo。
  await expect(page).toHaveURL('/builds')
  await expect(page.getByRole('button', { name: '打开用户菜单' })).toBeVisible()

  const refresh = (await context.cookies()).find(cookie => cookie.name === 'aegis_refresh')
  expect(refresh).toBeTruthy()
  expect(refresh!.httpOnly).toBe(true)
  expect(refresh!.sameSite).toBe('Strict')
  expect(await page.evaluate(() => document.cookie)).not.toContain('aegis_refresh')

  // 自动开通的账号固定为 member：可读项目/构建，但不能调用管理员接口。
  const status = await page.evaluate(async () => {
    const { apiRequest } = await import('/src/stores/session.ts')
    try {
      await apiRequest('/users')
      return 200
    } catch (error) {
      return (error as { status: number }).status
    }
  })
  expect(status).toBe(403)

  // 刷新后会话可恢复（refresh Cookie + /auth/me 真实链路）。
  await page.reload()
  await expect(page.getByRole('button', { name: '打开用户菜单' })).toBeVisible()
})
