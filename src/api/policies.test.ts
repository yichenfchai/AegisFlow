import { beforeEach, describe, expect, it, vi } from 'vitest'
import {
  policiesAPI,
  policyGateHints,
  REQUIRED_SCAN_VERDICT_TEXT,
  type ProjectPolicy,
} from './policies'
import { apiRequest } from '@/stores/session'
import { ApiError } from './http'

vi.mock('@/stores/session', () => ({ apiRequest: vi.fn() }))

const apiRequestMock = vi.mocked(apiRequest)

beforeEach(() => {
  apiRequestMock.mockReset()
})

const policy: ProjectPolicy = {
  projectId: 'proj/1',
  requireReview: true,
  requiredScanVerdict: 'passed',
  updatedBy: 'user-1',
  updatedByName: 'mira',
  updatedAt: 1_790_000_000,
}

describe('policiesAPI', () => {
  it('reads a project policy through the encoded project path', async () => {
    apiRequestMock.mockResolvedValueOnce(policy)
    await expect(policiesAPI.get('proj/1')).resolves.toEqual(policy)
    expect(apiRequestMock).toHaveBeenCalledWith('/projects/proj%2F1/policy')
  })

  it('replaces a project policy with PUT and both documented fields', async () => {
    apiRequestMock.mockResolvedValueOnce(policy)
    await policiesAPI.update('proj/1', { requireReview: true, requiredScanVerdict: 'passed' })
    expect(apiRequestMock).toHaveBeenCalledTimes(1)
    expect(apiRequestMock).toHaveBeenCalledWith('/projects/proj%2F1/policy', {
      method: 'PUT',
      body: JSON.stringify({ requireReview: true, requiredScanVerdict: 'passed' }),
    })
  })

  it('propagates the localized ApiError unchanged', async () => {
    const failure = new ApiError(403, 'forbidden', '当前账号没有执行该操作的权限')
    apiRequestMock.mockRejectedValueOnce(failure)
    await expect(
      policiesAPI.update('proj-1', { requireReview: false, requiredScanVerdict: 'none' }),
    ).rejects.toBe(failure)
  })
})

describe('REQUIRED_SCAN_VERDICT_TEXT', () => {
  it('localizes both stored values', () => {
    expect(REQUIRED_SCAN_VERDICT_TEXT).toEqual({
      none: '不要求',
      passed: '最新扫描必须通过',
    })
  })
})

describe('policyGateHints', () => {
  it('returns no hint for the default (off) policy, so the form stays unchanged', () => {
    expect(policyGateHints({ requireReview: false, requiredScanVerdict: 'none' })).toEqual([])
  })

  it('explains the review gate in Chinese', () => {
    const hints = policyGateHints({ requireReview: true, requiredScanVerdict: 'none' })
    expect(hints).toHaveLength(1)
    expect(hints[0]).toContain('已批准审查')
  })

  it('explains the scan gate in Chinese', () => {
    const hints = policyGateHints({ requireReview: false, requiredScanVerdict: 'passed' })
    expect(hints).toHaveLength(1)
    expect(hints[0]).toContain('最新扫描必须通过')
  })

  it('reports both hints when both gates are on', () => {
    expect(policyGateHints({ requireReview: true, requiredScanVerdict: 'passed' })).toHaveLength(2)
  })
})
