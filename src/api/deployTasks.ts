/** Real deployment-task and approval APIs (P4.2/P4.3); admin only. */
import { apiRequest } from '@/stores/session'
import type { Page } from '@/api/account'

export type DeploymentStatus =
  | 'queued'
  | 'awaiting_approval'
  | 'running'
  | 'succeeded'
  | 'failed'
  | 'cancelled'
  | 'interrupted'
  | 'rejected'

export type DeploymentEnvironment = 'development' | 'staging' | 'production'
export type ApprovalStatus = 'pending' | 'approved' | 'rejected' | 'expired'

export interface Deployment {
  id: string
  projectId: string
  targetId: string
  targetName: string
  targetEndpoint: string
  targetEnvironment: DeploymentEnvironment
  deploymentNumber: number
  status: DeploymentStatus
  version: string
  buildId: string
  rollbackOf: string
  approvalId: string
  triggeredBy: string
  detail: string
  createdAt: number
  startedAt: number
  finishedAt: number
}

export interface DeploymentEvent {
  id: number
  deploymentId: string
  stream: 'system' | 'response'
  message: string
  createdAt: number
}

export interface Approval {
  id: string
  deploymentId: string
  projectId: string
  status: ApprovalStatus
  requestedBy: string
  decidedBy: string
  reason: string
  createdAt: number
  expiresAt: number
  decidedAt: number
}

/** The subset of a deployment target the create form needs. */
export interface DeploymentTargetOption {
  id: string
  name: string
  kind: string
  environment: DeploymentEnvironment
  endpoint: string
  healthPath: string
  deployPath: string
  status: 'active' | 'disabled'
}

export interface DeployInput {
  targetId: string
  version: string
  buildId?: string
}

/**
 * Deployment tasks. `list` is the admin-wide page; `projectId` and `status` are
 * optional AND-combined filters, and paging stays server-side.
 */
export const deployTasksAPI = {
  list: ({
    projectId,
    status,
    limit = 20,
    offset = 0,
  }: {
    projectId?: string
    status?: DeploymentStatus
    limit?: number
    offset?: number
  } = {}) => {
    const params = new URLSearchParams({ limit: String(limit), offset: String(offset) })
    if (projectId) params.set('projectId', projectId)
    if (status) params.set('status', status)
    return apiRequest<Page<Deployment>>(`/deployments?${params.toString()}`)
  },
  deployment: (id: string) =>
    apiRequest<Deployment>(`/deployments/${encodeURIComponent(id)}`),
  create: (projectId: string, input: DeployInput) =>
    apiRequest<Deployment>(`/projects/${encodeURIComponent(projectId)}/deployments`, {
      method: 'POST',
      body: JSON.stringify(input),
    }),
  cancel: (id: string) =>
    apiRequest<Deployment>(`/deployments/${encodeURIComponent(id)}/cancel`, {
      method: 'POST',
      body: '{}',
    }),
  rollback: (id: string) =>
    apiRequest<Deployment>(`/deployments/${encodeURIComponent(id)}/rollback`, {
      method: 'POST',
      body: '{}',
    }),
  /** `after` is the exclusive event id cursor (same semantics as build logs). */
  events: (id: string, after = 0, limit = 200) =>
    apiRequest<{ items: DeploymentEvent[] }>(
      `/deployments/${encodeURIComponent(id)}/events?after=${after}&limit=${limit}`,
    ),
}

export const approvalsAPI = {
  approvals: (limit = 20, offset = 0, status?: ApprovalStatus) =>
    apiRequest<Page<Approval>>(
      `/approvals?limit=${limit}&offset=${offset}${status ? `&status=${encodeURIComponent(status)}` : ''}`,
    ),
  approve: (id: string) =>
    apiRequest<Approval>(`/approvals/${encodeURIComponent(id)}/approve`, {
      method: 'POST',
      body: '{}',
    }),
  reject: (id: string, reason?: string) =>
    apiRequest<Approval>(`/approvals/${encodeURIComponent(id)}/reject`, {
      method: 'POST',
      body: JSON.stringify(reason ? { reason } : {}),
    }),
}

/** Active and disabled targets; the create form keeps only the active ones. */
export function listTargets() {
  return apiRequest<Page<DeploymentTargetOption>>('/deployment-targets?limit=100&offset=0')
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

export const DEPLOYMENT_STATUS_TEXT: Record<DeploymentStatus, string> = {
  queued: '排队中',
  awaiting_approval: '等待审批',
  running: '运行中',
  succeeded: '成功',
  failed: '失败',
  cancelled: '已取消',
  interrupted: '已中断',
  rejected: '已拒绝',
}

export const DEPLOYMENT_STATUS_TONE: Record<
  DeploymentStatus,
  'success' | 'error' | 'running' | 'pending' | 'warning'
> = {
  queued: 'pending',
  awaiting_approval: 'warning',
  running: 'running',
  succeeded: 'success',
  failed: 'error',
  cancelled: 'pending',
  interrupted: 'pending',
  rejected: 'error',
}

export const APPROVAL_STATUS_TEXT: Record<ApprovalStatus, string> = {
  pending: '待审批',
  approved: '已批准',
  rejected: '已拒绝',
  expired: '已过期',
}

export const APPROVAL_STATUS_TONE: Record<ApprovalStatus, 'success' | 'error' | 'warning' | 'pending'> = {
  pending: 'warning',
  approved: 'success',
  rejected: 'error',
  expired: 'pending',
}

const TERMINAL_DEPLOYMENT_STATUSES: DeploymentStatus[] = [
  'succeeded',
  'failed',
  'cancelled',
  'interrupted',
  'rejected',
]

export function isDeploymentTerminal(status: DeploymentStatus): boolean {
  return TERMINAL_DEPLOYMENT_STATUSES.includes(status)
}
