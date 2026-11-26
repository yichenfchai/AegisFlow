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
  await page.evaluate(
    async ({ username }) => {
      const { apiRequest } = await import('/src/stores/session.ts')
      await apiRequest('/users', {
        method: 'POST',
        body: JSON.stringify({ username, password: 'Knowledge-pass-2026', displayName: username, role: 'member' }),
      })
    },
    { username },
  )
}

/**
 * 知识库（P5 批次 B）：管理员新建含 Markdown 与 `<script>` XSS payload 的文档 →
 * 列表搜索命中 → 详情里粗体/代码生效且脚本按文本显示（不弹窗、无 script 元素）→
 * 编辑标题与正文 → 删除后列表消失。所有数据经真实接口落库。
 */
test('知识库：新建 Markdown/XSS 文档、搜索、安全渲染详情、编辑与删除', async ({ page }) => {
  test.setTimeout(180_000)
  await login(page)
  const suffix = Date.now()
  const title = `发布手册-${suffix}`
  const updatedTitle = `${title}-v2`
  const xss = '<script>alert(1)</script>'
  const body = `# 发布手册\n\n**先备份再发布**\n\n执行 \`kubectl rollout undo\`。\n\n${xss}`
  const updatedBody = '更新后的正文：**安全渲染生效**。'

  // 侧栏入口进入真实知识库列表。
  await page.locator('aside').getByRole('link', { name: '知识库' }).click()
  await expect(page).toHaveURL('/knowledge')
  await expect(page.getByRole('heading', { level: 1, name: '知识库' })).toBeVisible()

  // 新建：标题、标签、正文（Markdown 粗体/行内代码 + XSS payload）。
  await page.getByRole('button', { name: '新建文档' }).click()
  const createDialog = page.getByRole('dialog')
  await expect(createDialog.getByRole('heading', { name: '新建知识库文档' })).toBeVisible()
  await createDialog.getByLabel('标题', { exact: true }).fill(title)
  await createDialog.getByLabel('标签', { exact: true }).fill('ops, runbook')
  await createDialog.getByLabel('正文', { exact: true }).fill(body)
  await createDialog.getByRole('button', { name: '创建文档' }).click()
  await expect(page.getByText(`已创建文档「${title}」`)).toBeVisible()

  // 搜索命中（服务端 q 过滤）。
  await page.getByLabel('搜索标题', { exact: true }).fill(title)
  await page.getByRole('button', { name: '搜索', exact: true }).click()
  const row = page.getByRole('row').filter({ hasText: title })
  await expect(row).toBeVisible({ timeout: 20_000 })
  await expect(row.getByText('ops')).toBeVisible()
  await expect(row.getByText('runbook')).toBeVisible()

  // 详情：粗体与行内代码生效，XSS payload 以文本显示且不执行、不生成 script 元素。
  let dialogMessage = ''
  page.on('dialog', (dialog) => {
    dialogMessage = dialog.message()
    void dialog.dismiss()
  })
  await row.getByRole('link', { name: title }).click()
  await expect(page).toHaveURL(/\/knowledge\/[A-Za-z0-9_-]{16,64}$/)
  await expect(page.getByRole('heading', { level: 1, name: title })).toBeVisible()
  const bold = page.locator('strong', { hasText: '先备份再发布' })
  await expect(bold).toBeVisible()
  const fontWeight = await bold.evaluate((element) => Number(getComputedStyle(element).fontWeight))
  expect(fontWeight).toBeGreaterThanOrEqual(600)
  await expect(page.locator('code', { hasText: 'kubectl rollout undo' })).toBeVisible()
  await expect(page.getByText(xss, { exact: false })).toBeVisible()
  await expect(page.locator('main script, section script')).toHaveCount(0)
  expect(dialogMessage).toBe('')

  // 编辑标题与正文后详情立即反映新内容（粗体仍生效）。
  await page.getByRole('button', { name: '编辑' }).click()
  const editDialog = page.getByRole('dialog')
  await expect(editDialog.getByRole('heading', { name: '编辑知识库文档' })).toBeVisible()
  await editDialog.getByLabel('标题', { exact: true }).fill(updatedTitle)
  await editDialog.getByLabel('正文', { exact: true }).fill(updatedBody)
  await editDialog.getByRole('button', { name: '保存修改' }).click()
  await expect(page.getByRole('heading', { level: 1, name: updatedTitle })).toBeVisible()
  await expect(page.locator('strong', { hasText: '安全渲染生效' })).toBeVisible()

  // 删除后列表不再出现该文档。
  await page.locator('aside').getByRole('link', { name: '知识库' }).click()
  await expect(page).toHaveURL('/knowledge')
  await page.getByLabel('搜索标题', { exact: true }).fill(updatedTitle)
  await page.getByRole('button', { name: '搜索', exact: true }).click()
  const updatedRow = page.getByRole('row').filter({ hasText: updatedTitle })
  await expect(updatedRow).toBeVisible()
  await updatedRow.getByRole('button', { name: '删除' }).click()
  const deleteDialog = page.getByRole('dialog')
  await expect(deleteDialog.getByRole('heading', { name: '删除知识库文档' })).toBeVisible()
  await deleteDialog.getByRole('button', { name: '确认删除' }).click()
  await expect(page.getByText(`已删除文档「${updatedTitle}」`)).toBeVisible()
  await expect(page.getByRole('row').filter({ hasText: updatedTitle })).toHaveCount(0)
})

/**
 * 密钥管理（P5 批次 B，admin-only）：新建后列表只显示名称/元数据，页面与接口响应
 * 都不含值；轮换新值（表内「最近轮换」变化、旧值不可见）；删除后消失。非 admin
 * 无导航入口、`/keys` 被守卫拒绝、接口 403，而知识库读取仍开放。
 */
test('密钥管理：写-only 新建/轮换/删除，页面与接口不回显值，非 admin 受限', async ({ page }) => {
  test.setTimeout(180_000)
  await login(page)
  const suffix = Date.now()
  const secretName = `e2e-secret-${suffix}`
  const secretValue = 'super-secret-value-123'
  const rotatedValue = 'rotated-super-secret-456'

  await page.locator('aside').getByRole('link', { name: '密钥管理' }).click()
  await expect(page).toHaveURL('/keys')
  await expect(page.getByRole('heading', { level: 1, name: '密钥管理' })).toBeVisible()

  // 新建：name + value 密码框 + 二次确认；保存后值不可再查看。
  await page.getByRole('button', { name: '新建密钥' }).click()
  const createDialog = page.getByRole('dialog')
  await expect(createDialog.getByRole('heading', { name: '新建密钥' })).toBeVisible()
  await createDialog.getByLabel('名称', { exact: true }).fill(secretName)
  await createDialog.getByLabel('值', { exact: true }).fill(secretValue)
  await createDialog.getByLabel('确认值', { exact: true }).fill(secretValue)
  await createDialog.getByRole('button', { name: '保存密钥' }).click()
  await expect(page.getByText(`已保存密钥「${secretName}」`)).toBeVisible()

  const secretRow = page.getByRole('row').filter({ hasText: secretName })
  await expect(secretRow).toBeVisible({ timeout: 20_000 })
  await expect(secretRow).toContainText('e2e-admin')
  await expect(secretRow.getByText('未轮换')).toBeVisible()
  // 页面任何位置都不得出现明文值。
  await expect(page.locator('body')).not.toContainText(secretValue)

  // 接口响应也不得包含值：列表、创建、轮换、删除逐一检查 JSON。
  const apiProbe = await page.evaluate(
    async ({ suffix }) => {
      const { apiRequest } = await import('/src/stores/session.ts')
      const detailValue = 'api-probe-value-789'
      const rotatedDetailValue = 'api-probe-rotated-012'
      const created = await apiRequest<{ id: string }>('/secrets', {
        method: 'POST',
        body: JSON.stringify({ name: `api-secret-${suffix}`, value: detailValue }),
      })
      const createdJSON = JSON.stringify(created)
      const listJSON = JSON.stringify(await apiRequest('/secrets?limit=100&offset=0'))
      const rotatedJSON = JSON.stringify(
        await apiRequest(`/secrets/${encodeURIComponent(created.id)}`, {
          method: 'PUT',
          body: JSON.stringify({ value: rotatedDetailValue }),
        }),
      )
      const deletedJSON = JSON.stringify(
        await apiRequest(`/secrets/${encodeURIComponent(created.id)}`, { method: 'DELETE' }),
      )
      return {
        createdJSON,
        listJSON,
        rotatedJSON,
        deletedJSON,
        leaked: [createdJSON, listJSON, rotatedJSON, deletedJSON].some(
          (payload) => payload.includes(detailValue) || payload.includes(rotatedDetailValue),
        ),
      }
    },
    { suffix },
  )
  expect(apiProbe.leaked).toBe(false)
  await expect(page.locator('body')).not.toContainText(secretValue)

  // 轮换：新值二次确认；表内「最近轮换」由未轮换变为时间，页面仍不含任何值。
  // created/updated 都是整秒时间戳，等待 1 秒以上让轮换时间与创建时间可区分。
  await page.waitForTimeout(1100)
  await secretRow.getByRole('button', { name: '轮换值' }).click()
  const rotateDialog = page.getByRole('dialog')
  await expect(rotateDialog.getByRole('heading', { name: `轮换密钥「${secretName}」` })).toBeVisible()
  await rotateDialog.getByLabel('新值', { exact: true }).fill(rotatedValue)
  await rotateDialog.getByLabel('确认值', { exact: true }).fill(rotatedValue)
  await rotateDialog.getByRole('button', { name: '确认轮换' }).click()
  await expect(page.getByText(`已轮换密钥「${secretName}」`)).toBeVisible()
  const rotatedRow = page.getByRole('row').filter({ hasText: secretName })
  await expect(rotatedRow.getByText('未轮换')).toHaveCount(0)
  await expect(page.locator('body')).not.toContainText(rotatedValue)

  // 删除后行消失。
  await rotatedRow.getByRole('button', { name: '删除' }).click()
  const deleteDialog = page.getByRole('dialog')
  await expect(deleteDialog.getByRole('heading', { name: '删除密钥' })).toBeVisible()
  await deleteDialog.getByRole('button', { name: '确认删除' }).click()
  await expect(page.getByText(`已删除密钥「${secretName}」`)).toBeVisible()
  await expect(page.getByRole('row').filter({ hasText: secretName })).toHaveCount(0)

  // ---- 非 admin：密钥无入口、路由守卫拒绝、接口 403；知识库可读但不可写 ----
  const docTitle = `只读知识库-${suffix}`
  const docID = await page.evaluate(
    async ({ title }) => {
      const { apiRequest } = await import('/src/stores/session.ts')
      const created = await apiRequest<{ id: string }>('/knowledge', {
        method: 'POST',
        body: JSON.stringify({ title, body: '成员可读的 **Markdown** 正文' }),
      })
      return created.id
    },
    { title: docTitle },
  )
  const memberName = `knowledge-member-${suffix}`
  await createMember(page, memberName)
  const browser = page.context().browser()!
  const memberCtx = await browser.newContext()
  const member = await memberCtx.newPage()
  await login(member, memberName, 'Knowledge-pass-2026')

  // 密钥管理：导航隐藏 + 路由守卫 + 服务端 403（中文文案来自统一错误码映射）。
  await expect(member.locator('aside').getByRole('link', { name: '密钥管理' })).toHaveCount(0)
  await member.goto('/keys')
  await expect(member.getByRole('heading', { name: '无权访问' })).toBeVisible()
  const forbidden = await member.evaluate(async () => {
    const { apiRequest } = await import('/src/stores/session.ts')
    try {
      await apiRequest('/secrets?limit=20&offset=0')
      return { status: 200, message: '' }
    } catch (error) {
      const apiError = error as { status: number; message: string }
      return { status: apiError.status, message: apiError.message }
    }
  })
  expect(forbidden.status).toBe(403)
  expect(forbidden.message).toBe('当前账号没有执行该操作的权限')
  const knowledgeWrite = await member.evaluate(async () => {
    const { apiRequest } = await import('/src/stores/session.ts')
    try {
      await apiRequest('/knowledge', { method: 'POST', body: JSON.stringify({ title: 'nope', body: 'x' }) })
      return 200
    } catch (error) {
      return (error as { status: number }).status
    }
  })
  expect(knowledgeWrite).toBe(403)

  // 知识库读取对所有已登录角色开放：成员能看列表、详情与安全渲染的 Markdown，但没有写按钮。
  await member.locator('aside').getByRole('link', { name: '知识库' }).click()
  await expect(member).toHaveURL('/knowledge')
  await expect(member.getByRole('heading', { level: 1, name: '知识库' })).toBeVisible()
  await expect(member.getByRole('button', { name: '新建文档' })).toHaveCount(0)
  await member.goto(`/knowledge/${docID}`)
  await expect(member.getByRole('heading', { level: 1, name: docTitle })).toBeVisible()
  await expect(member.locator('strong', { hasText: 'Markdown' })).toBeVisible()
  await expect(member.getByRole('button', { name: '编辑' })).toHaveCount(0)
  await expect(member.getByRole('button', { name: '删除' })).toHaveCount(0)
  await memberCtx.close()
})
