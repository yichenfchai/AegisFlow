/** Real scan-result registry; scan writes are signed machine webhooks handled by the backend. */
import { apiRequest } from '@/stores/session'
import type { Page } from '@/api/account'

/** Verdict reported by an external scanning tool; display-only evidence. */
export type ScanVerdict = 'passed' | 'failed' | 'warning'

/**
 * One immutable scan result written back by a signed external tool. `tool` and
 * `version` are empty when the delivery omitted them; `reportUrl` is absent when
 * absent and is display-only (the server never fetches it).
 */
export interface Scan {
  id: string
  projectId: string
  /** Project name resolved by the server from the project registry. */
  projectName: string
  /** External tool name, 1-64 bytes on one line. */
  source: string
  /** Optional tool name, at most 64 bytes on one line. */
  tool: string
  /** Optional tool version, at most 64 bytes on one line. */
  version: string
  verdict: ScanVerdict
  /** At most 1000 bytes; empty when the delivery carried no note. */
  summary: string
  /** Absent or an absolute http(s) URL without userinfo or fragment. */
  reportUrl?: string
  /** Unix seconds when the server accepted the delivery. */
  receivedAt: number
}

export interface ScanListQuery {
  projectId?: string
  verdict?: ScanVerdict
  limit?: number
  offset?: number
}

/**
 * Query string shared by the global and project-scoped scan endpoints. Empty
 * filters are omitted and paging is always present; `verdict` is ASCII and
 * `projectId` is encoded because ids may contain URL-reserved characters.
 */
export function scanListQuery({ projectId, verdict, limit = 20, offset = 0 }: ScanListQuery): string {
  const parts = [`limit=${limit}`, `offset=${offset}`]
  if (projectId) parts.push(`projectId=${encodeURIComponent(projectId)}`)
  if (verdict) parts.push(`verdict=${verdict}`)
  return parts.join('&')
}

/**
 * Scan API. `listGlobal` covers every project the caller can see (admin: all;
 * other roles: public projects plus their own/member projects), while
 * `listByProject` pages one visible project. `get` answers 404 for an invisible
 * project, exactly like the project itself.
 */
export const scansAPI = {
  listGlobal: (query: ScanListQuery = {}) =>
    apiRequest<Page<Scan>>(`/scans?${scanListQuery(query)}`),
  listByProject: (projectId: string, query: Omit<ScanListQuery, 'projectId'> = {}) =>
    apiRequest<Page<Scan>>(`/projects/${encodeURIComponent(projectId)}/scans?${scanListQuery(query)}`),
  get: (id: string) => apiRequest<Scan>(`/scans/${encodeURIComponent(id)}`),
}

/** Chinese verdict labels: passed=通过 / failed=失败 / warning=警告. */
export const SCAN_VERDICT_TEXT: Record<ScanVerdict, string> = {
  passed: '通过',
  failed: '失败',
  warning: '警告',
}

/** Badge tone per verdict: passed green / failed red / warning amber. */
export const SCAN_VERDICT_TONE: Record<ScanVerdict, 'success' | 'error' | 'warning'> = {
  passed: 'success',
  failed: 'error',
  warning: 'warning',
}
