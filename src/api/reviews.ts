/** Real review-center API; project visibility and reviewer overlays are enforced server-side. */
import { apiRequest } from '@/stores/session'
import type { Page } from '@/api/account'

export type ReviewStatus = 'pending' | 'approved' | 'rejected' | 'cancelled'

/** Verdict reported by an external scanner; display-only evidence. */
export type ReviewVerdict = 'passed' | 'failed' | 'warning'

/**
 * One immutable external result written back by a signed external tool (E4).
 * External results are evidence only: they never change the review status or
 * the deployment gate. `summary` preserves newlines and may be empty; `url` is
 * empty when absent and is display-only (the server never fetches it).
 */
export interface ReviewExternalResult {
  id: string
  reviewId: string
  deliveryId: string
  /** External tool name, 1-64 bytes on one line. */
  source: string
  verdict: ReviewVerdict
  /** At most 500 bytes; empty when the delivery carried no note. */
  summary: string
  /** Empty or an absolute http(s) URL without userinfo or fragment. */
  url: string
  /** Unix seconds when the server accepted the delivery. */
  receivedAt: number
}

export interface ReviewExternalResultList {
  items: ReviewExternalResult[]
}

/**
 * One gate-style change review. `decidedBy` / `decidedByName` / `decidedAt` /
 * `reason` are only present once a decision (or cancellation) exists; a pending
 * review omits them. The review body is display-only context: the server never
 * fetches `externalUrl`.
 */
export interface Review {
  id: string
  projectId: string
  /** Per-project monotonic sequence allocated under the project row lock. */
  number: number
  subject: string
  description: string
  version: string
  externalUrl: string
  status: ReviewStatus
  requestedBy: string
  requestedByName: string
  decidedBy?: string
  decidedByName?: string
  decidedAt?: number
  reason?: string
  createdAt: number
  updatedAt: number
}

export interface CreateReviewInput {
  /** Required; 3-200 UTF-8 bytes on one line. */
  subject: string
  /** Optional; at most 2000 UTF-8 bytes. */
  description?: string
  /** Optional; at most 64 UTF-8 bytes on one line. */
  version?: string
  /** Optional absolute http(s) link without userinfo or fragment. */
  externalUrl?: string
}

export interface ReviewListQuery {
  projectId?: string
  status?: ReviewStatus
  limit?: number
  offset?: number
}

/**
 * Query string shared by the global and project-scoped list endpoints. Empty
 * filters are omitted and paging is always present; `status` values are ASCII
 * and `projectId` is encoded because ids may contain URL-reserved characters.
 */
export function reviewListQuery({ projectId, status, limit = 20, offset = 0 }: ReviewListQuery): string {
  const parts = [`limit=${limit}`, `offset=${offset}`]
  if (projectId) parts.push(`projectId=${encodeURIComponent(projectId)}`)
  if (status) parts.push(`status=${status}`)
  return parts.join('&')
}

/**
 * Review API. `listGlobal` covers every project the caller can see (admin: all;
 * other roles: public projects plus their own/member projects), while
 * `listByProject` pages one visible project. Decide/cancel authorization is
 * re-checked on every request; the UI only mirrors it.
 */
export const reviewsAPI = {
  listGlobal: (query: ReviewListQuery = {}) =>
    apiRequest<Page<Review>>(`/reviews?${reviewListQuery(query)}`),
  listByProject: (projectId: string, query: Omit<ReviewListQuery, 'projectId'> = {}) =>
    apiRequest<Page<Review>>(
      `/projects/${encodeURIComponent(projectId)}/reviews?${reviewListQuery(query)}`,
    ),
  create: (projectId: string, input: CreateReviewInput) =>
    apiRequest<Review>(`/projects/${encodeURIComponent(projectId)}/reviews`, {
      method: 'POST',
      body: JSON.stringify(input),
    }),
  get: (id: string) => apiRequest<Review>(`/reviews/${encodeURIComponent(id)}`),
  /** Optional reason; an empty reason is omitted from the body. */
  approve: (id: string, reason?: string) =>
    apiRequest<Review>(`/reviews/${encodeURIComponent(id)}/approve`, {
      method: 'POST',
      body: JSON.stringify(reason ? { reason } : {}),
    }),
  /** The reason is required by the server; reject omits it only when the UI failed to validate. */
  reject: (id: string, reason: string) =>
    apiRequest<Review>(`/reviews/${encodeURIComponent(id)}/reject`, {
      method: 'POST',
      body: JSON.stringify({ reason }),
    }),
  cancel: (id: string) =>
    apiRequest<Review>(`/reviews/${encodeURIComponent(id)}/cancel`, {
      method: 'POST',
      body: '{}',
    }),
  /** Read-only external evidence for a visible review, newest first (server caps at 50). */
  listExternalResults: (id: string) =>
    apiRequest<ReviewExternalResultList>(
      `/reviews/${encodeURIComponent(id)}/external-results`,
    ),
}

export const REVIEW_STATUS_TEXT: Record<ReviewStatus, string> = {
  pending: '待审查',
  approved: '已批准',
  rejected: '已拒绝',
  cancelled: '已取消',
}

export const REVIEW_STATUS_TONE: Record<
  ReviewStatus,
  'success' | 'error' | 'warning' | 'pending'
> = {
  pending: 'warning',
  approved: 'success',
  rejected: 'error',
  cancelled: 'pending',
}

/** Display label for a review option in pickers: "#12 subject · v1.2.3". */
export function reviewOptionLabel(review: Pick<Review, 'number' | 'subject' | 'version'>): string {
  return `#${review.number} ${review.subject}${review.version ? ` · ${review.version}` : ''}`
}

/** Chinese verdict labels for external results: passed=通过 / failed=失败 / warning=警告. */
export const REVIEW_VERDICT_TEXT: Record<ReviewVerdict, string> = {
  passed: '通过',
  failed: '失败',
  warning: '警告',
}

/** Badge tone per verdict: passed green / failed red / warning amber. */
export const REVIEW_VERDICT_TONE: Record<ReviewVerdict, 'success' | 'error' | 'warning'> = {
  passed: 'success',
  failed: 'error',
  warning: 'warning',
}
