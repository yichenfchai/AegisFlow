/** Audit CSV export: same filters as the audit list, downloaded as a Blob. */
import { apiBlobRequest } from '@/stores/session'
import type { AuditFilters } from '@/api/account'

/**
 * Query string shared with the export endpoint. Only the six documented filters
 * are sent (no paging: the server caps the export and reports truncation), empty
 * values are omitted and every value is percent-encoded because actor names and
 * RFC3339 timestamps may contain reserved characters.
 */
export function auditExportQuery(filters: AuditFilters): string {
  const parts: string[] = []
  const keys = ['action', 'objectType', 'result', 'actor', 'from', 'to'] as const
  for (const key of keys) {
    const value = filters[key]
    if (value) parts.push(`${key}=${encodeURIComponent(value)}`)
  }
  return parts.join('&')
}

/** Local-time filename with the export date and time, e.g. audit-20261127-004500.csv. */
export function auditExportFilename(now: Date = new Date()): string {
  const pad = (value: number) => String(value).padStart(2, '0')
  return `audit-${now.getFullYear()}${pad(now.getMonth() + 1)}${pad(now.getDate())}-${pad(now.getHours())}${pad(now.getMinutes())}${pad(now.getSeconds())}.csv`
}

export interface AuditExport {
  blob: Blob
  /** True when more than 10000 rows matched; the UI asks the user to narrow filters. */
  truncated: boolean
  /** Suggested download filename (the server also sets Content-Disposition). */
  filename: string
}

export const auditAPI = {
  /** Downloads the filtered audit trail; the caller triggers the browser download. */
  exportCsv: async (filters: AuditFilters = {}, now: Date = new Date()): Promise<AuditExport> => {
    const query = auditExportQuery(filters)
    const { blob, headers } = await apiBlobRequest(
      `/audit/export.csv${query ? `?${query}` : ''}`,
    )
    return {
      blob,
      truncated: headers.get('X-Audit-Truncated') === 'true',
      filename: auditExportFilename(now),
    }
  },
}

/** Triggers a browser download for a Blob and releases the object URL afterwards. */
export function downloadBlob(blob: Blob, filename: string): void {
  const url = URL.createObjectURL(blob)
  const anchor = document.createElement('a')
  anchor.href = url
  anchor.download = filename
  anchor.rel = 'noopener'
  document.body.appendChild(anchor)
  anchor.click()
  anchor.remove()
  // Revoke after the navigation task starts; the Blob stays valid until then.
  setTimeout(() => URL.revokeObjectURL(url), 0)
}
