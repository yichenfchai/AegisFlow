import { beforeEach, describe, expect, it, vi } from 'vitest'
import {
  REVIEW_STATUS_TEXT,
  REVIEW_VERDICT_TEXT,
  REVIEW_VERDICT_TONE,
  reviewsAPI,
  reviewListQuery,
  reviewOptionLabel,
} from './reviews'
import { apiRequest } from '@/stores/session'
import { ApiError } from './http'

vi.mock('@/stores/session', () => ({ apiRequest: vi.fn() }))

const apiRequestMock = vi.mocked(apiRequest)

beforeEach(() => {
  apiRequestMock.mockReset()
})

describe('reviewListQuery', () => {
  it('always carries server-side paging and omits empty filters', () => {
    expect(reviewListQuery({})).toBe('limit=20&offset=0')
    expect(reviewListQuery({ limit: 100, offset: 40 })).toBe('limit=100&offset=40')
  })

  it('includes a status filter without encoding it', () => {
    expect(reviewListQuery({ status: 'approved', limit: 5, offset: 0 })).toBe(
      'limit=5&offset=0&status=approved',
    )
  })

  it('percent-encodes a project id filter', () => {
    expect(reviewListQuery({ projectId: 'a/b+c', status: 'pending' })).toBe(
      'limit=20&offset=0&projectId=a%2Fb%2Bc&status=pending',
    )
  })

  it('omits an explicitly empty project id and status', () => {
    expect(reviewListQuery({ projectId: '', status: undefined })).toBe('limit=20&offset=0')
  })
})

describe('reviewOptionLabel', () => {
  it('appends the optional version to the picker label', () => {
    expect(reviewOptionLabel({ number: 12, subject: '发布 v1.2.3', version: 'v1.2.3' })).toBe(
      '#12 发布 v1.2.3 · v1.2.3',
    )
  })

  it('keeps the label stable when the version is empty', () => {
    expect(reviewOptionLabel({ number: 3, subject: '修复登录', version: '' })).toBe('#3 修复登录')
  })
})

describe('REVIEW_STATUS_TEXT', () => {
  it('localizes every review status', () => {
    expect(REVIEW_STATUS_TEXT).toEqual({
      pending: '待审查',
      approved: '已批准',
      rejected: '已拒绝',
      cancelled: '已取消',
    })
  })
})

const externalResult = {
  id: 'er-1',
  reviewId: 'rv/1',
  deliveryId: 'e2e-delivery-1',
  source: 'e2e-scanner',
  verdict: 'warning' as const,
  summary: '扫描完成\n共 3 条提示',
  url: 'https://example.com/report/1',
  receivedAt: 1_790_000_000,
}

describe('reviewsAPI.listExternalResults', () => {
  it('requests the external results of the encoded review id and passes the list through', async () => {
    apiRequestMock.mockResolvedValueOnce({ items: [externalResult] })
    await expect(reviewsAPI.listExternalResults('rv/1')).resolves.toEqual({ items: [externalResult] })
    expect(apiRequestMock).toHaveBeenCalledTimes(1)
    expect(apiRequestMock).toHaveBeenCalledWith('/reviews/rv%2F1/external-results')
  })

  it('propagates the localized ApiError so the page can render the shared Chinese text', async () => {
    const failure = new ApiError(404, 'not_found', '请求的资源不存在，或你没有查看权限')
    apiRequestMock.mockRejectedValueOnce(failure)
    await expect(reviewsAPI.listExternalResults('rv-1')).rejects.toBe(failure)
  })
})

describe('REVIEW_VERDICT_TEXT', () => {
  it('localizes every external verdict and maps it to the documented badge tone', () => {
    expect(REVIEW_VERDICT_TEXT).toEqual({ passed: '通过', failed: '失败', warning: '警告' })
    expect(REVIEW_VERDICT_TONE).toEqual({ passed: 'success', failed: 'error', warning: 'warning' })
  })
})
