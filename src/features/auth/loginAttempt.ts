/**
 * 登录页的工作空间（租户）逻辑（阶段 F3）。
 *
 * 后端契约：登录请求可选 `tenantSlug`；未提供时用户名/邮箱必须在跨租户范围内
 * 唯一，否则返回 400 `tenant_required`（中文提示由 errors.ts 统一映射）。本模块
 * 把「校验 + 调用登录」抽成纯函数，登录页只负责渲染与状态切换，便于在 node
 * 环境下做单元测试；它不依赖 React、DOM 或 session store。
 */
import { ApiError } from '@/api/errors'

/**
 * 与后端 `tenants.ValidSlug` 完全一致的格式：1-32 个小写 ASCII 字母/数字，
 * 短横线只能在中间。提交前 trim，空值表示「不指定工作空间」。
 */
const TENANT_SLUG_PATTERN = /^[a-z0-9](?:[a-z0-9-]{0,30}[a-z0-9])?$/

/** `tenant_required` 的固定中文说明，与 api/errors.ts 的映射保持一致。 */
export const TENANT_REQUIRED_TEXT = '该邮箱存在于多个工作空间，请输入工作空间标识'
export const TENANT_SLUG_REQUIRED_TEXT = '请输入工作空间标识'
export const TENANT_SLUG_INVALID_TEXT =
  '工作空间标识只能包含小写字母、数字和短横线（1-32 个字符，且不能以短横线开头或结尾）'

/** 去掉首尾空白；是否合法由 isValidTenantSlug 判断。 */
export function normalizeTenantSlug(raw: string): string {
  return raw.trim()
}

/** 工作空间标识是否符合后端的 slug 契约（空值不算合法，由调用方决定是否必填）。 */
export function isValidTenantSlug(value: string): boolean {
  return TENANT_SLUG_PATTERN.test(value)
}

/**
 * 从登录页 URL 的 `?tenant=<slug>` 读取预填值。非法值一律忽略，避免地址栏
 * 里的任意文本伪造工作空间状态；合法值由调用方决定是否展开输入框。
 */
export function tenantSlugFromSearch(search: string): string {
  const raw = new URLSearchParams(search).get('tenant')
  if (!raw) return ''
  const slug = normalizeTenantSlug(raw)
  return isValidTenantSlug(slug) ? slug : ''
}

/** 登录失败是否为「需要工作空间标识」：仅认 400 tenant_required。 */
export function isTenantRequiredError(error: unknown): boolean {
  return error instanceof ApiError && error.code === 'tenant_required'
}

/**
 * 登录依赖注入签名，与 useSession().login 一致：最后一个参数为空时表示不指定
 * 工作空间（单租户登录契约不变）。用函数参数注入让单元测试不需要 Web Locks、
 * fetch 或全局 store。
 */
export type LoginFn = (
  username: string,
  password: string,
  remember: boolean,
  tenantSlug?: string,
) => Promise<void>

export interface LoginAttempt {
  username: string
  password: string
  remember: boolean
  tenantSlug: string
  /** 收到 tenant_required 后必须补填工作空间标识才允许再次提交。 */
  tenantSlugRequired: boolean
}

export type LoginOutcome =
  | { kind: 'success' }
  | { kind: 'invalid'; message: string }
  | { kind: 'tenant_required'; error: unknown }
  | { kind: 'failed'; error: unknown }

/**
 * 执行一次登录：先做与后端一致的本地校验，校验通过才调用 login。
 * - 工作空间标识非法：不发起请求，返回 invalid（避免无意义的一次 400）；
 * - 上一次是 tenant_required 但标识为空：提示补填，不重复触发歧义错误；
 * - 后端返回 tenant_required：返回独立 kind，调用方保持已填邮箱/密码并展开输入框；
 * - 其它错误：交给统一错误码中文映射。
 */
export async function runLoginAttempt(attempt: LoginAttempt, login: LoginFn): Promise<LoginOutcome> {
  const username = attempt.username.trim()
  const tenantSlug = normalizeTenantSlug(attempt.tenantSlug)
  if (!username) return { kind: 'invalid', message: '用户名不能为空' }
  if (!attempt.password) return { kind: 'invalid', message: '密码不能为空' }
  if (tenantSlug !== '' && !isValidTenantSlug(tenantSlug)) {
    return { kind: 'invalid', message: TENANT_SLUG_INVALID_TEXT }
  }
  if (attempt.tenantSlugRequired && tenantSlug === '') {
    return { kind: 'invalid', message: TENANT_SLUG_REQUIRED_TEXT }
  }
  try {
    // 省略 tenantSlug（而不是发送空串）才是单租户既有契约；后端的严格 JSON
    // 校验会拒绝空串并返回 400 validation_failed。
    await login(username, attempt.password, attempt.remember, tenantSlug || undefined)
    return { kind: 'success' }
  } catch (error) {
    if (isTenantRequiredError(error)) return { kind: 'tenant_required', error }
    return { kind: 'failed', error }
  }
}
