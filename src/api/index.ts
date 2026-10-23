/** Auth always uses the server. Unimplemented business APIs require explicit demo opt-in. */
import { api as mockApi } from './mock'
import type { Api } from './mock'
import { ApiError } from './http'

export const businessMocksEnabled = import.meta.env.VITE_ENABLE_BUSINESS_MOCKS === 'true'
const unavailable = async (): Promise<never> => {
  throw new ApiError(501, 'not_implemented', '此业务服务尚未接入，当前仅开放真实账号鉴权')
}
const realApi: Api = {
  projects: unavailable, builds: unavailable, deploys: unavailable, reviews: unavailable,
  approvals: unavailable, knowledge: unavailable, auditLogs: unavailable, defenseEvents: unavailable,
  secrets: unavailable, deployTargets: unavailable, scanFindings: unavailable,
}
export const api: Api = businessMocksEnabled ? mockApi : realApi
export * from './types'
