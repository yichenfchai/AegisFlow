/** Real defense-event registry; ingest is a signed machine webhook, reads/ack are admin-only. */
import { apiRequest } from '@/stores/session'
import type { Page } from '@/api/account'

/** Severity reported by an external detection source. */
export type SecuritySeverity = 'low' | 'medium' | 'high' | 'critical'

/** An event starts open and only an admin acknowledgement closes it. */
export type SecurityEventStatus = 'open' | 'acknowledged'

/**
 * One defense event written back by a signed external detection tool.
 * Acknowledgement fields are only present once an admin acknowledged it;
 * `detailsUrl` is absent when absent and is display-only.
 */
export interface SecurityEvent {
  id: string
  /** External detection source, 1-64 bytes on one line. */
  source: string
  severity: SecuritySeverity
  /** Event type, 1-64 bytes on one line. */
  eventType: string
  /** At most 1000 bytes; newlines preserved. */
  summary: string
  status: SecurityEventStatus
  /** Absent or an absolute http(s) URL without userinfo or fragment. */
  detailsUrl?: string
  /** Unix seconds when the source observed the event. */
  occurredAt: number
  /** Unix seconds when the server accepted the delivery. */
  receivedAt: number
  /** Admin actor id; absent while open. */
  acknowledgedBy?: string
  /** Acknowledging admin username; absent while open. */
  acknowledgedByName?: string
  /** Unix seconds of the acknowledgement; absent while open. */
  acknowledgedAt?: number
}

export interface SecurityEventListQuery {
  severity?: SecuritySeverity
  status?: SecurityEventStatus
  source?: string
  limit?: number
  offset?: number
}

/**
 * Query string for the admin defense-event list. Empty filters are omitted and
 * paging is always present; `source` is percent-encoded because a source name
 * may contain reserved characters.
 */
export function securityEventListQuery({
  severity,
  status,
  source,
  limit = 20,
  offset = 0,
}: SecurityEventListQuery): string {
  const parts = [`limit=${limit}`, `offset=${offset}`]
  if (severity) parts.push(`severity=${severity}`)
  if (status) parts.push(`status=${status}`)
  if (source) parts.push(`source=${encodeURIComponent(source)}`)
  return parts.join('&')
}

/**
 * Defense-event API. Every call is admin-only server-side: the route guard
 * hides the module from other roles, and the API still answers 403.
 */
export const securityEventsAPI = {
  list: (query: SecurityEventListQuery = {}) =>
    apiRequest<Page<SecurityEvent>>(`/security-events?${securityEventListQuery(query)}`),
  get: (id: string) => apiRequest<SecurityEvent>(`/security-events/${encodeURIComponent(id)}`),
  /** Open -> acknowledged; repeating the call is idempotent server-side. */
  ack: (id: string) =>
    apiRequest<SecurityEvent>(`/security-events/${encodeURIComponent(id)}/ack`, {
      method: 'POST',
      body: '{}',
    }),
}

/** Chinese severity labels: low=低 / medium=中 / high=高 / critical=严重. */
export const SECURITY_SEVERITY_TEXT: Record<SecuritySeverity, string> = {
  low: '低',
  medium: '中',
  high: '高',
  critical: '严重',
}

/** Badge tone per severity: low neutral / medium amber / high+critical red. */
export const SECURITY_SEVERITY_TONE: Record<SecuritySeverity, 'neutral' | 'warning' | 'error'> = {
  low: 'neutral',
  medium: 'warning',
  high: 'error',
  critical: 'error',
}

export const SECURITY_EVENT_STATUS_TEXT: Record<SecurityEventStatus, string> = {
  open: '开放',
  acknowledged: '已确认',
}

export const SECURITY_EVENT_STATUS_TONE: Record<SecurityEventStatus, 'pending' | 'success'> = {
  open: 'pending',
  acknowledged: 'success',
}
