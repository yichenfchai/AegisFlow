/**
 * LDAP 目录认证的登录页逻辑（阶段 G）。
 *
 * 后端契约：未配置 `AEGIS_LDAP_URL` 时 `/auth/ldap/config` 返回
 * `{enabled:false,label:''}`，登录页必须与纯密码登录完全一致；启用时登录页显示
 * 「{label}」入口，展开后的表单提交 `{username,password,tenantSlug?}`。本模块把探针
 * 归一、错误映射与提交校验抽成纯函数，登录页只负责渲染与状态切换，便于在 node
 * 环境下做单元测试；它不依赖 React 或 session store。
 */
import { ApiError, apiErrorText, errorText } from '@/api/errors'
import { ldapAPI } from '@/api/http'
import type { LDAPConfig } from '@/api/http'
import {
  TENANT_REQUIRED_TEXT,
  TENANT_SLUG_INVALID_TEXT,
  TENANT_SLUG_REQUIRED_TEXT,
  isTenantRequiredError,
  isValidTenantSlug,
  normalizeTenantSlug,
} from './loginAttempt'

/** 目录不可达/超时/服务账号绑定失败时后端的 503，登录页给出专用中文提示。 */
export const LDAP_UNAVAILABLE_TEXT = '目录服务暂不可用，请稍后重试'
/** 目录口令错误与未知目录用户统一为通用文案，不泄露差异（后端同为 401）。 */
export const LDAP_INVALID_CREDENTIALS_TEXT = '用户名或密码错误'
export const LDAP_USERNAME_REQUIRED_TEXT = '目录用户名不能为空'
export const LDAP_PASSWORD_REQUIRED_TEXT = '密码不能为空'
export const LDAP_DEFAULT_LABEL = 'LDAP 登录'

/** 未配置时返回 false，登录页不渲染任何目录元素。 */
export function ldapEnabled(config: LDAPConfig | null): boolean {
  return Boolean(config?.enabled)
}

/** 目录入口文案；未启用返回 null（调用方据此完全不渲染入口）。 */
export function ldapButtonLabel(config: LDAPConfig | null): string | null {
  if (!config?.enabled) return null
  const label = config.label.trim()
  return label || LDAP_DEFAULT_LABEL
}

/**
 * 加载目录可用性。网络错误、5xx、旧后端（404）都归一为未启用：目录认证是可选
 * 能力，探针失败不能让密码登录页报错或卡住。
 */
export async function loadLDAPConfig(): Promise<LDAPConfig> {
  try {
    const config = await ldapAPI.config()
    if (typeof config?.enabled !== 'boolean' || typeof config.label !== 'string') {
      return { enabled: false, label: '' }
    }
    return config
  } catch {
    return { enabled: false, label: '' }
  }
}

/**
 * LDAP 登录失败 → 用户可见中文文案。401 统一为「用户名或密码错误」；503 为
 * 「目录服务暂不可用」；其余（account_not_found、account_disabled、tenant_required、
 * 校验失败等）复用 errors.ts 的统一错误码映射，绝不渲染后端英文 message。
 */
export function ldapLoginErrorMessage(error: unknown): string {
  if (error instanceof ApiError) {
    if (error.status === 401) return LDAP_INVALID_CREDENTIALS_TEXT
    if (error.status === 503) return LDAP_UNAVAILABLE_TEXT
    if (error.code === 'tenant_required') return TENANT_REQUIRED_TEXT
    // 只按错误码查统一中文表，绝不回显 ApiError 携带的原始 message。
    return apiErrorText(error.code, error.retryAfter)
  }
  return errorText(error)
}

/** 登录依赖注入签名，与 useSession().loginLDAP 一致。 */
export type LDAPLoginFn = (username: string, password: string, tenantSlug?: string) => Promise<void>

export interface LDAPLoginAttempt {
  username: string
  password: string
  tenantSlug: string
  /** 收到 tenant_required 后必须补填工作空间标识才允许再次提交。 */
  tenantSlugRequired: boolean
}

export type LDAPLoginOutcome =
  | { kind: 'success' }
  | { kind: 'invalid'; message: string }
  | { kind: 'tenant_required'; error: unknown }
  | { kind: 'failed'; error: unknown }

/**
 * 执行一次目录登录：先做与后端一致的本地校验，校验通过才调用 login。
 * - 目录用户名 trim 后不能为空；密码不能为空（后端 1-1024 字节）；
 * - 工作空间标识非法时不发起请求；上次 tenant_required 但标识为空时提示补填；
 * - 后端 tenant_required 返回独立 kind，调用方保留输入并展开工作空间标识；
 * - 其它错误交给统一错误码中文映射（在 ldapLoginErrorMessage 中）。
 */
export async function runLDAPLoginAttempt(
  attempt: LDAPLoginAttempt,
  login: LDAPLoginFn,
): Promise<LDAPLoginOutcome> {
  const username = attempt.username.trim()
  const tenantSlug = normalizeTenantSlug(attempt.tenantSlug)
  if (!username) return { kind: 'invalid', message: LDAP_USERNAME_REQUIRED_TEXT }
  if (!attempt.password) return { kind: 'invalid', message: LDAP_PASSWORD_REQUIRED_TEXT }
  if (tenantSlug !== '' && !isValidTenantSlug(tenantSlug)) {
    return { kind: 'invalid', message: TENANT_SLUG_INVALID_TEXT }
  }
  if (attempt.tenantSlugRequired && tenantSlug === '') {
    return { kind: 'invalid', message: TENANT_SLUG_REQUIRED_TEXT }
  }
  try {
    await login(username, attempt.password, tenantSlug || undefined)
    return { kind: 'success' }
  } catch (error) {
    if (isTenantRequiredError(error)) return { kind: 'tenant_required', error }
    return { kind: 'failed', error }
  }
}
