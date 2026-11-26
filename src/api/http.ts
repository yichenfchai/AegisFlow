import {
  ApiError,
  NETWORK_ERROR_TEXT,
  TIMEOUT_ERROR_TEXT,
  apiErrorText,
} from './errors'

export { ApiError } from './errors'

export interface SessionUser {
  id: string
  username: string
  displayName: string
  role: 'admin' | 'member' | 'viewer'
}
export interface AuthSession {
  user: SessionUser
  accessToken: string
  expiresIn: number
}
// A relative URL keeps refresh cookies on the application's origin, including in development.
const baseURL = (import.meta.env.VITE_API_BASE_URL ?? '/api/v1').replace(/\/$/, '')
/** 单次请求上限；超时与网络故障都归一为客户端 ApiError，文案由 errors.ts 统一提供。 */
const REQUEST_TIMEOUT_MS = 15_000

/** name 判定的结构化检查：DOMException 不是 Error 子类，浏览器里只能按 name 识别。 */
function isNamedError(cause: unknown, name: string): boolean {
  return (
    typeof cause === 'object' &&
    cause !== null &&
    'name' in cause &&
    (cause as { name: unknown }).name === name
  )
}

/** Retry-After 只接受非负整数秒；HTTP-date 等形式忽略，退回通用限流文案。 */
function parseRetryAfter(value: string | null): number | undefined {
  if (!value) return undefined
  const seconds = Number.parseInt(value, 10)
  return Number.isFinite(seconds) && seconds >= 0 ? seconds : undefined
}

/**
 * 统一发起 fetch：超时、主动取消与网络故障都归一为客户端 ApiError，文案由
 * errors.ts 提供。JSON 与 Blob 两类请求共用同一套网络错误语义。
 */
async function fetchApi(path: string, init: RequestInit, headers: Headers): Promise<Response> {
  const timeoutSignal = init.signal ? undefined : AbortSignal.timeout(REQUEST_TIMEOUT_MS)
  try {
    return await fetch(`${baseURL}${path}`, {
      ...init,
      headers,
      credentials: 'include',
      signal: init.signal ?? timeoutSignal,
    })
  } catch (cause) {
    // 后端英文错误 message 不进入任何展示路径；这里只区分超时、主动取消与网络故障。
    if (isNamedError(cause, 'TimeoutError') || timeoutSignal?.aborted) {
      throw new ApiError(0, 'timeout', TIMEOUT_ERROR_TEXT)
    }
    if (init.signal?.aborted || isNamedError(cause, 'AbortError')) {
      throw new ApiError(0, 'request_aborted', apiErrorText('request_aborted'))
    }
    throw new ApiError(0, 'network_error', NETWORK_ERROR_TEXT)
  }
}

interface ErrorPayload {
  error?: { code: string; message: string }
  requestId?: string
}

/** 把已解析的错误封套归一为 ApiError；只信任 code 与 requestId。 */
function apiErrorFromPayload(response: Response, payload: ErrorPayload): ApiError {
  const code = payload.error?.code ?? 'request_failed'
  const retryAfter =
    response.status === 429 ? parseRetryAfter(response.headers.get('Retry-After')) : undefined
  return new ApiError(response.status, code, apiErrorText(code, retryAfter), payload.requestId, retryAfter)
}

/**
 * 把失败响应归一为 ApiError。非 JSON 错误体（代理/网关页）退回通用
 * request_failed 文案，绝不渲染响应正文。
 */
async function errorFromResponse(response: Response): Promise<ApiError> {
  let payload: ErrorPayload = {}
  try {
    payload = await response.json()
  } catch {
    // A non-JSON error body (proxy/page) still maps to the shared fallback text.
  }
  return apiErrorFromPayload(response, payload)
}

export async function httpRequest<T>(path: string, init: RequestInit = {}, token?: string): Promise<T> {
  const headers = new Headers(init.headers)
  headers.set('Accept', 'application/json')
  if (init.body !== undefined) headers.set('Content-Type', 'application/json')
  if (token) headers.set('Authorization', `Bearer ${token}`)
  const response = await fetchApi(path, init, headers)
  let payload: { data?: T; error?: { code: string; message: string }; requestId?: string }
  try {
    payload = await response.json()
  } catch {
    throw new ApiError(response.status, 'invalid_response', apiErrorText('invalid_response'))
  }
  if (!response.ok) {
    throw apiErrorFromPayload(response, payload)
  }
  if (!('data' in payload)) {
    throw new ApiError(response.status, 'invalid_response', apiErrorText('invalid_response'), payload.requestId)
  }
  return payload.data as T
}

/** Blob 下载结果：响应头用于读取 X-Audit-Truncated 这类元信息。 */
export interface BlobPayload {
  blob: Blob
  headers: Headers
}

/**
 * 下载型请求（例如审计 CSV 导出）：成功时返回 Blob 与响应头，失败时与
 * httpRequest 一样把统一 JSON 错误封套归一为 ApiError。鉴权 token 由会话层注入。
 */
export async function httpBlobRequest(
  path: string,
  init: RequestInit = {},
  token?: string,
): Promise<BlobPayload> {
  const headers = new Headers(init.headers)
  headers.set('Accept', 'text/csv')
  if (token) headers.set('Authorization', `Bearer ${token}`)
  const response = await fetchApi(path, init, headers)
  if (!response.ok) {
    throw await errorFromResponse(response)
  }
  return { blob: await response.blob(), headers: response.headers }
}
export const authAPI = {
  login: (username: string, password: string, remember: boolean) => httpRequest<AuthSession>('/auth/login', { method: 'POST', body: JSON.stringify({ username, password, remember }) }),
  refresh: () => httpRequest<AuthSession>('/auth/refresh', { method: 'POST', body: '{}' }),
  logout: () => httpRequest<{ loggedOut: boolean }>('/auth/logout', { method: 'POST', body: '{}' }),
  me: (token: string) => httpRequest<SessionUser>('/auth/me', {}, token),
}

/**
 * OIDC 单点登录可用性探针（公开、GET）。未配置时后端返回
 * `{enabled:false,label:''}`，不会泄露 issuer/client id；登录页据此决定是否
 * 渲染 SSO 入口，未启用时页面保持纯密码登录，不显示任何 SSO 占位。
 */
export interface OIDCConfig {
  enabled: boolean
  label: string
}

export const oidcAPI = {
  config: () => httpRequest<OIDCConfig>('/auth/oidc/config'),
}
