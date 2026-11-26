/** Real per-project automation-policy API; gate writes are admin/maintainer only. */
import { apiRequest } from '@/stores/session'

/** The scan-gate vocabulary: none disables it, passed requires the latest scan to pass. */
export type RequiredScanVerdict = 'none' | 'passed'

/**
 * One project's automation policy. A project without a stored row reports the
 * defaults (requireReview=false, requiredScanVerdict=none, updatedBy empty,
 * updatedAt 0), which is also the pre-policy deployment behavior.
 */
export interface ProjectPolicy {
  projectId: string
  /** When true every deployment and rollback must reference an approved review. */
  requireReview: boolean
  /** When passed, the project's latest scan verdict must be passed. */
  requiredScanVerdict: RequiredScanVerdict
  /** Last updater actor id; empty when no policy row is stored. */
  updatedBy: string
  /** Last updater username resolved for display; absent when unavailable. */
  updatedByName?: string
  /** Unix seconds of the last update; 0 when no policy row is stored. */
  updatedAt: number
}

export interface PolicyInput {
  requireReview: boolean
  requiredScanVerdict: RequiredScanVerdict
}

export const policiesAPI = {
  get: (projectId: string) =>
    apiRequest<ProjectPolicy>(`/projects/${encodeURIComponent(projectId)}/policy`),
  update: (projectId: string, input: PolicyInput) =>
    apiRequest<ProjectPolicy>(`/projects/${encodeURIComponent(projectId)}/policy`, {
      method: 'PUT',
      body: JSON.stringify(input),
    }),
}

export const REQUIRED_SCAN_VERDICT_TEXT: Record<RequiredScanVerdict, string> = {
  none: '不要求',
  passed: '最新扫描必须通过',
}

/**
 * Chinese gate warnings for the deployment form and the policy page. Empty means
 * the policy adds no requirement, so the default (off) experience is unchanged.
 */
export function policyGateHints(
  policy: Pick<ProjectPolicy, 'requireReview' | 'requiredScanVerdict'>,
): string[] {
  const hints: string[] = []
  if (policy.requireReview) {
    hints.push('该项目策略要求关联已批准审查：创建与回滚部署都必须选择一条本项目已批准的审查。')
  }
  if (policy.requiredScanVerdict === 'passed') {
    hints.push('该项目策略要求最新扫描必须通过：请先由外部扫描工具回写一条通过结论，再进行部署。')
  }
  return hints
}
