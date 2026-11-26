import { beforeEach, describe, expect, it, vi } from 'vitest'
import {
  SCAN_VERDICT_TEXT,
  SCAN_VERDICT_TONE,
  scansAPI,
  scanListQuery,
} from './scans'
import { apiRequest } from '@/stores/session'
import { ApiError } from './http'

vi.mock('@/stores/session', () => ({ apiRequest: vi.fn() }))

const apiRequestMock = vi.mocked(apiRequest)

beforeEach(() => {
  apiRequestMock.mockReset()
})

describe('scanListQuery', () => {
  it('always carries server-side paging and omits empty filters', () => {
    expect(scanListQuery({})).toBe('limit=20&offset=0')
    expect(scanListQuery({ limit: 100, offset: 40 })).toBe('limit=100&offset=40')
  })

  it('includes a verdict filter without encoding it', () => {
    expect(scanListQuery({ verdict: 'failed', limit: 5, offset: 0 })).toBe(
      'limit=5&offset=0&verdict=failed',
    )
  })

  it('percent-encodes a project id filter', () => {
    expect(scanListQuery({ projectId: 'a/b+c', verdict: 'passed' })).toBe(
      'limit=20&offset=0&projectId=a%2Fb%2Bc&verdict=passed',
    )
  })

  it('omits an explicitly empty project id and verdict', () => {
    expect(scanListQuery({ projectId: '', verdict: undefined })).toBe('limit=20&offset=0')
  })
})

const scan = {
  id: 'scan-1',
  projectId: 'proj/1',
  projectName: '演示项目',
  source: 'e2e-scanner',
  tool: 'trivy',
  version: '0.55.0',
  verdict: 'passed' as const,
  summary: '扫描通过\n共 0 个问题',
  reportUrl: 'https://example.com/reports/1',
  receivedAt: 1_790_000_000,
}

describe('scansAPI', () => {
  it('requests the global scan page and passes it through', async () => {
    const page = { items: [scan], total: 1, limit: 12, offset: 0 }
    apiRequestMock.mockResolvedValueOnce(page)
    await expect(
      scansAPI.listGlobal({ projectId: 'proj/1', verdict: 'passed', limit: 12, offset: 0 }),
    ).resolves.toEqual(page)
    expect(apiRequestMock).toHaveBeenCalledTimes(1)
    expect(apiRequestMock).toHaveBeenCalledWith(
      '/scans?limit=12&offset=0&projectId=proj%2F1&verdict=passed',
    )
  })

  it('requests a project-scoped scan page with the encoded project id', async () => {
    apiRequestMock.mockResolvedValueOnce({ items: [], total: 0, limit: 20, offset: 0 })
    await scansAPI.listByProject('proj/1', { verdict: 'warning' })
    expect(apiRequestMock).toHaveBeenCalledWith(
      '/projects/proj%2F1/scans?limit=20&offset=0&verdict=warning',
    )
  })

  it('requests the detail of the encoded scan id and passes the scan through', async () => {
    apiRequestMock.mockResolvedValueOnce(scan)
    await expect(scansAPI.get('scan/1')).resolves.toEqual(scan)
    expect(apiRequestMock).toHaveBeenCalledWith('/scans/scan%2F1')
  })

  it('propagates the localized ApiError so the page can render the shared Chinese text', async () => {
    const failure = new ApiError(404, 'not_found', '请求的资源不存在，或你没有查看权限')
    apiRequestMock.mockRejectedValueOnce(failure)
    await expect(scansAPI.get('scan-1')).rejects.toBe(failure)
  })
})

describe('SCAN_VERDICT_TEXT', () => {
  it('localizes every verdict and maps it to the documented badge tone', () => {
    expect(SCAN_VERDICT_TEXT).toEqual({ passed: '通过', failed: '失败', warning: '警告' })
    expect(SCAN_VERDICT_TONE).toEqual({ passed: 'success', failed: 'error', warning: 'warning' })
  })
})
