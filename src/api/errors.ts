/**
 * API 错误的统一展示层（P2-19）。
 *
 * 后端的 `error.message` 是给服务端日志用的英文内部说明，绝不能渲染给用户：
 * 客户端只按 `error.code` 查中文文案。原始 code 仍保留在 `ApiError.code` 上供
 * 程序判断（例如 401 触发刷新、`origin_denied` 提示来源限制），未知 code 一律
 * 退化为通用中文兜底。所有面向用户的错误路径都应通过 `errorText()` 取文案，
 * 不要直接展示响应体里的 message（开发模式下同样如此，避免泄露内部细节）。
 */

/**
 * 客户端统一抛出的 API 错误。
 *
 * `status` / `code` / `requestId` 供程序判断与问题排查；`message` 始终是本地化
 * 后的中文文案，不是后端原始 message。429 响应额外携带 `retryAfter`（秒）。
 */
export class ApiError extends Error {
  readonly status: number
  readonly code: string
  readonly requestId?: string
  readonly retryAfter?: number

  constructor(status: number, code: string, message: string, requestId?: string, retryAfter?: number) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.code = code
    this.requestId = requestId
    this.retryAfter = retryAfter
  }
}

export const NETWORK_ERROR_TEXT = '无法连接服务，请检查网络或稍后重试'
export const TIMEOUT_ERROR_TEXT = '请求超时，请稍后重试'
export const FALLBACK_ERROR_TEXT = '请求失败，请稍后重试'

/** 429 的 Retry-After 上限：异常的超长等待值只保留一小时，避免提示无意义。 */
const MAX_RETRY_AFTER_SECONDS = 3600

/**
 * 错误码 → 用户可见中文文案。键为后端 `error.code`（见 backend/README.md
 * 「常见错误」）或客户端传输层错误码。新增后端错误码时在此补充；漏补只会
 * 退化为通用兜底，不会把英文原文泄露到界面。
 */
export const API_ERROR_TEXT: Readonly<Record<string, string>> = {
  unauthorized: '登录状态已失效，请重新登录',
  forbidden: '当前账号没有执行该操作的权限',
  not_found: '请求的资源不存在，或你没有查看权限',
  validation_failed: '提交的内容不合法，请检查后重试',
  invalid_request: '请求参数不合法，请检查后重试',
  conflict: '数据状态已发生变化或存在冲突，请刷新后重试',
  rate_limited: '操作过于频繁，请稍后重试',
  account_disabled: '账号已被禁用，请联系管理员',
  origin_denied: '当前请求来源不被允许，请从本站页面重试',
  not_ready: '服务尚未就绪，请稍后重试',
  dependencies_unavailable: '服务依赖暂不可用，请稍后重试',
  // 独立密钥管理：服务端未配置 AEGIS_SECRET_KEY 时拒绝写入；页面用统一中文提示说明可操作项。
  secrets_not_configured:
    '密钥存储未配置：请在服务端设置 AEGIS_SECRET_KEY 后重启；列表与删除仍可使用。',
  internal_error: '服务器内部错误，请稍后重试',
  unsupported_media_type: '请求格式不受支持，请刷新页面后重试',
  // 客户端传输层 / 响应解析错误码
  network_error: NETWORK_ERROR_TEXT,
  timeout: TIMEOUT_ERROR_TEXT,
  request_aborted: '请求已取消',
  invalid_response: '服务返回了无效响应，请稍后重试',
  request_failed: FALLBACK_ERROR_TEXT,
}

/** 按错误码取中文文案；rate_limited 且带 Retry-After 时给出等待秒数。 */
export function apiErrorText(code: string, retryAfter?: number): string {
  if (
    code === 'rate_limited' &&
    typeof retryAfter === 'number' &&
    Number.isFinite(retryAfter) &&
    retryAfter > 0
  ) {
    const seconds = Math.min(Math.ceil(retryAfter), MAX_RETRY_AFTER_SECONDS)
    return `操作过于频繁，请 ${seconds} 秒后重试`
  }
  return API_ERROR_TEXT[code] ?? FALLBACK_ERROR_TEXT
}

/**
 * 任意异常 → 用户可见中文文案。`ApiError` 在构造时已按 code 本地化；其他前端
 * 异常（会话锁、日志流等）保留自身中文说明；非 Error 或主动取消使用兜底文案。
 */
export function errorText(error: unknown, fallback: string = FALLBACK_ERROR_TEXT): string {
  if (error instanceof ApiError) return error.message
  if (error instanceof Error && error.name !== 'AbortError' && error.message) return error.message
  return fallback
}
