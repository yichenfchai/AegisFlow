/** Real deployment-target registry API (P4.1). Admin only. */
import { apiRequest } from '@/stores/session'
import type { Page } from '@/api/account'
import { fmtRel } from '@/api/format'

export type DeploymentEnvironment = 'development' | 'staging' | 'production'
export type DeploymentTargetKind = 'http'
export type DeploymentTargetStatus = 'active' | 'disabled'

export interface DeploymentTarget {
  id: string
  name: string
  kind: DeploymentTargetKind
  environment: DeploymentEnvironment
  endpoint: string
  healthPath: string
  /** Whether a credential is stored; the secret itself is never returned. */
  hasSecret: boolean
  status: DeploymentTargetStatus
  createdBy: string
  createdAt: number
  updatedAt: number
  /** Unix seconds of the last probe; 0 means never probed. */
  lastCheckAt: number
  lastCheckOk: boolean
  /** "ok", "http <code>", "timeout" or a sanitized network error. */
  lastCheckDetail: string
}

export interface DeploymentTargetInput {
  name: string
  kind?: DeploymentTargetKind
  environment?: DeploymentEnvironment
  endpoint: string
  healthPath?: string
  secret?: string
  status?: DeploymentTargetStatus
}

/** Omitting secret keeps the stored credential; "" clears it. */
export interface DeploymentTargetPatch {
  name?: string
  environment?: DeploymentEnvironment
  endpoint?: string
  healthPath?: string
  secret?: string
  status?: DeploymentTargetStatus
}

export interface DeploymentTargetCheck {
  ok: boolean
  statusCode: number
  latencyMs: number
  detail: string
  checkedAt: number
}

export const deploymentTargetsAPI = {
  targets: (limit = 20, offset = 0) =>
    apiRequest<Page<DeploymentTarget>>(`/deployment-targets?limit=${limit}&offset=${offset}`),
  target: (id: string) =>
    apiRequest<DeploymentTarget>(`/deployment-targets/${encodeURIComponent(id)}`),
  create: (input: DeploymentTargetInput) =>
    apiRequest<DeploymentTarget>('/deployment-targets', { method: 'POST', body: JSON.stringify(input) }),
  update: (id: string, patch: DeploymentTargetPatch) =>
    apiRequest<DeploymentTarget>(`/deployment-targets/${encodeURIComponent(id)}`, {
      method: 'PATCH',
      body: JSON.stringify(patch),
    }),
  remove: (id: string) =>
    apiRequest<{ removed: boolean }>(`/deployment-targets/${encodeURIComponent(id)}`, { method: 'DELETE' }),
  check: (id: string) =>
    apiRequest<DeploymentTargetCheck>(`/deployment-targets/${encodeURIComponent(id)}/check`, {
      method: 'POST',
      body: '{}',
    }),
}

export const DEPLOY_ENVIRONMENT_TEXT: Record<DeploymentEnvironment, string> = {
  development: '开发',
  staging: '预发布',
  production: '生产',
}

export const DEPLOY_ENVIRONMENT_TONE: Record<DeploymentEnvironment, 'success' | 'warning' | 'neutral'> = {
  development: 'neutral',
  staging: 'warning',
  production: 'success',
}

export const DEPLOY_TARGET_STATUS_TEXT: Record<DeploymentTargetStatus, string> = {
  active: '启用',
  disabled: '已禁用',
}

export const DEPLOY_TARGET_STATUS_TONE: Record<DeploymentTargetStatus, 'success' | 'pending'> = {
  active: 'success',
  disabled: 'pending',
}

/**
 * Stored last-check summary for lists and detail. The API keeps the outcome
 * detail (ok/http code/timeout/network error) and the check time, but not the
 * latency; a fresh probe's status code and latency are shown on the detail page.
 */
export function lastCheckText(
  target: Pick<DeploymentTarget, 'lastCheckAt' | 'lastCheckOk' | 'lastCheckDetail'>,
): string {
  if (!target.lastCheckAt) return '未检查'
  const detail = target.lastCheckOk
    ? target.lastCheckDetail && target.lastCheckDetail !== 'ok'
      ? target.lastCheckDetail
      : 'HTTP 2xx'
    : target.lastCheckDetail || '检查失败'
  return `${target.lastCheckOk ? '健康' : '异常'} · ${detail} · ${fmtRel(target.lastCheckAt * 1000)}`
}
