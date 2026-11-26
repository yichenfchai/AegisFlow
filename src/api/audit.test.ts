import { beforeEach, describe, expect, it, vi } from 'vitest'
import { auditAPI, auditExportFilename, auditExportQuery } from './audit'
import { apiBlobRequest } from '@/stores/session'
import { ApiError } from './http'
import type { AuditFilters } from './account'

vi.mock('@/stores/session', () => ({ apiBlobRequest: vi.fn() }))

const apiBlobMock = vi.mocked(apiBlobRequest)

beforeEach(() => {
  apiBlobMock.mockReset()
})

function blobResponse(truncated = false) {
  return {
    blob: new Blob(['id,createdAt\r\n'], { type: 'text/csv' }),
    headers: new Headers(truncated ? { 'X-Audit-Truncated': 'true' } : {}),
  }
}

describe('auditExportQuery', () => {
  it('is empty with no filters so the path stays clean', () => {
    expect(auditExportQuery({})).toBe('')
    expect(auditExportQuery({ action: '', actor: '' })).toBe('')
  })

  it('carries every filter in the documented order and percent-encodes values', () => {
    const filters: AuditFilters = {
      action: 'user.create',
      objectType: 'project',
      result: 'failure',
      actor: 'mira@example.com',
      from: '2026-11-01T00:00:00Z',
      to: '2026-11-30',
    }
    expect(auditExportQuery(filters)).toBe(
      'action=user.create&objectType=project&result=failure&actor=mira%40example.com&from=2026-11-01T00%3A00%3A00Z&to=2026-11-30',
    )
  })
})

describe('auditExportFilename', () => {
  it('includes the local date and time of the export', () => {
    // Month is zero-based in the Date constructor; the filename uses 1-based months.
    expect(auditExportFilename(new Date(2026, 10, 27, 0, 45, 30))).toBe(
      'audit-20261127-004530.csv',
    )
    expect(auditExportFilename(new Date(2026, 0, 2, 9, 5, 7))).toBe('audit-20260102-090507.csv')
  })
})

describe('auditAPI.exportCsv', () => {
  it('requests the export path without a query when no filter is set', async () => {
    const response = blobResponse()
    apiBlobMock.mockResolvedValueOnce(response)
    const result = await auditAPI.exportCsv({}, new Date(2026, 10, 27, 0, 45, 30))
    expect(apiBlobMock).toHaveBeenCalledWith('/audit/export.csv')
    expect(result.blob).toBe(response.blob)
    expect(result.filename).toBe('audit-20261127-004530.csv')
    expect(result.truncated).toBe(false)
  })

  it('passes the current filters and surfaces the truncation header', async () => {
    apiBlobMock.mockResolvedValueOnce(blobResponse(true))
    const result = await auditAPI.exportCsv({ action: 'policy.update', actor: 'mira' })
    expect(apiBlobMock).toHaveBeenCalledWith(
      '/audit/export.csv?action=policy.update&actor=mira',
    )
    expect(result.truncated).toBe(true)
  })

  it('propagates the localized ApiError so the page can show the Chinese message', async () => {
    const failure = new ApiError(403, 'forbidden', '当前账号没有执行该操作的权限')
    apiBlobMock.mockRejectedValueOnce(failure)
    await expect(auditAPI.exportCsv()).rejects.toBe(failure)
  })
})
