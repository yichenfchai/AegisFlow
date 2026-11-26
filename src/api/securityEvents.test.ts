import { beforeEach, describe, expect, it, vi } from 'vitest'
import {
  SECURITY_EVENT_STATUS_TEXT,
  SECURITY_EVENT_STATUS_TONE,
  SECURITY_SEVERITY_TEXT,
  SECURITY_SEVERITY_TONE,
  securityEventListQuery,
  securityEventsAPI,
} from './securityEvents'
import { apiRequest } from '@/stores/session'
import { ApiError } from './http'

vi.mock('@/stores/session', () => ({ apiRequest: vi.fn() }))

const apiRequestMock = vi.mocked(apiRequest)

beforeEach(() => {
  apiRequestMock.mockReset()
})

describe('securityEventListQuery', () => {
  it('always carries server-side paging and omits empty filters', () => {
    expect(securityEventListQuery({})).toBe('limit=20&offset=0')
    expect(securityEventListQuery({ limit: 50, offset: 10 })).toBe('limit=50&offset=10')
  })

  it('includes severity and status filters without encoding them', () => {
    expect(securityEventListQuery({ severity: 'critical', status: 'open' })).toBe(
      'limit=20&offset=0&severity=critical&status=open',
    )
  })

  it('percent-encodes the optional source filter', () => {
    expect(securityEventListQuery({ source: 'waf/edge 1' })).toBe(
      'limit=20&offset=0&source=waf%2Fedge%201',
    )
  })
})

const event = {
  id: 'event-1',
  source: 'waf',
  severity: 'high' as const,
  eventType: 'sql_injection',
  summary: '拦截到注入尝试',
  status: 'open' as const,
  detailsUrl: 'https://example.com/events/1',
  occurredAt: 1_790_000_000,
  receivedAt: 1_790_000_010,
}

describe('securityEventsAPI', () => {
  it('requests the admin event page and passes it through', async () => {
    const page = { items: [event], total: 1, limit: 12, offset: 0 }
    apiRequestMock.mockResolvedValueOnce(page)
    await expect(
      securityEventsAPI.list({ severity: 'high', status: 'open', limit: 12 }),
    ).resolves.toEqual(page)
    expect(apiRequestMock).toHaveBeenCalledWith(
      '/security-events?limit=12&offset=0&severity=high&status=open',
    )
  })

  it('requests the detail of the encoded event id', async () => {
    apiRequestMock.mockResolvedValueOnce(event)
    await expect(securityEventsAPI.get('event/1')).resolves.toEqual(event)
    expect(apiRequestMock).toHaveBeenCalledWith('/security-events/event%2F1')
  })

  it('posts an empty acknowledgement body to the encoded event id', async () => {
    const acked = { ...event, status: 'acknowledged' as const, acknowledgedBy: 'admin-1' }
    apiRequestMock.mockResolvedValueOnce(acked)
    await expect(securityEventsAPI.ack('event/1')).resolves.toEqual(acked)
    expect(apiRequestMock).toHaveBeenCalledWith('/security-events/event%2F1/ack', {
      method: 'POST',
      body: '{}',
    })
  })

  it('propagates the localized ApiError so the page can render the shared Chinese text', async () => {
    const failure = new ApiError(403, 'forbidden', '当前账号没有执行该操作的权限')
    apiRequestMock.mockRejectedValueOnce(failure)
    await expect(securityEventsAPI.list()).rejects.toBe(failure)
  })
})

describe('defense labels', () => {
  it('localizes every severity and maps it to a badge tone', () => {
    expect(SECURITY_SEVERITY_TEXT).toEqual({ low: '低', medium: '中', high: '高', critical: '严重' })
    expect(SECURITY_SEVERITY_TONE).toEqual({
      low: 'neutral',
      medium: 'warning',
      high: 'error',
      critical: 'error',
    })
  })

  it('localizes every status and maps it to a badge tone', () => {
    expect(SECURITY_EVENT_STATUS_TEXT).toEqual({ open: '开放', acknowledged: '已确认' })
    expect(SECURITY_EVENT_STATUS_TONE).toEqual({ open: 'pending', acknowledged: 'success' })
  })
})
