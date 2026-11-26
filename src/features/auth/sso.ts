/**
 * OIDC 单点登录的登录页逻辑（P5 批次 D）。
 *
 * 与后端契约保持一致：未配置 OIDC 时页面行为必须与纯密码登录完全一致。
 * 因此这里把「探针加载失败」也归一为未启用，而不是显示错误或占位按钮。
 * 所有函数都是纯逻辑或仅依赖 fetch，便于在 node 环境下做单元测试。
 */
import { oidcAPI } from '@/api/http'
import type { OIDCConfig } from '@/api/http'
import { safeReturnTo } from './returnTo'

export const SSO_UNAVAILABLE_TEXT = '单点登录暂时不可用，请稍后重试或使用密码登录'

/** 未配置时返回 false，登录页不渲染任何 SSO 元素。 */
export function ssoEnabled(config: OIDCConfig | null): boolean {
  return Boolean(config?.enabled)
}

/** SSO 按钮文案；未启用返回 null（调用方据此完全不渲染按钮）。 */
export function ssoButtonLabel(config: OIDCConfig | null): string | null {
  if (!config?.enabled) return null
  const label = config.label.trim()
  return label || '企业单点登录'
}

/**
 * 加载 OIDC 可用性。网络错误、5xx、旧后端（404）都归一为未启用：
 * 单点登录是可选能力，探针失败不能让密码登录页报错或卡住。
 */
export async function loadOIDCConfig(): Promise<OIDCConfig> {
  try {
    const config = await oidcAPI.config()
    if (typeof config?.enabled !== 'boolean' || typeof config.label !== 'string') {
      return { enabled: false, label: '' }
    }
    return config
  } catch {
    return { enabled: false, label: '' }
  }
}

/**
 * 生成同窗口跳转的 SSO 入口。redirectTo 只允许站内相对路径（复用
 * safeReturnTo），避免把登录成功后的跳转变成开放重定向。
 */
export function ssoStartURL(returnTo: string | null): string {
  return `/api/v1/auth/oidc/start?redirectTo=${encodeURIComponent(safeReturnTo(returnTo))}`
}

const SSO_ERROR_TEXT: Readonly<Record<string, string>> = {
  state_expired: '单点登录状态已过期，请重试',
  token_exchange_failed: SSO_UNAVAILABLE_TEXT,
  invalid_token: '单点登录返回的身份信息无效，请联系管理员',
  email_unverified: '单点登录账号邮箱未验证，请联系管理员',
  account_not_found: '账号未开通，请联系管理员',
  account_disabled: '账号已被禁用，请联系管理员',
  server_error: SSO_UNAVAILABLE_TEXT,
}

/**
 * 把后端回调重定向携带的 sso_error 映射为中文提示。未知编码退化为通用
 * 文案，绝不渲染后端或 IdP 的原始错误信息。
 */
export function ssoErrorMessage(code: string | null | undefined): string | null {
  if (!code) return null
  return SSO_ERROR_TEXT[code] ?? SSO_UNAVAILABLE_TEXT
}
