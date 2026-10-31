import { describe, expect, it } from 'vitest'
import { auditQueryString, type AuditFilters } from './account'

describe('auditQueryString', () => {
  it('always sends limit and offset when no filter is active', () => {
    expect(auditQueryString({}, 20, 0)).toBe('limit=20&offset=0')
    expect(auditQueryString({}, 10, 30)).toBe('limit=10&offset=30')
  })

  it('adds a single filter', () => {
    expect(auditQueryString({ action: 'user.create' }, 10, 0)).toBe(
      'limit=10&offset=0&action=user.create',
    )
  })

  it('combines all filters in a stable order', () => {
    const filters: AuditFilters = {
      action: 'build.trigger',
      objectType: 'build',
      result: 'failure',
      actor: 'alice',
      from: '2026-10-01',
      to: '2026-10-31T23:59:59Z',
    }
    expect(auditQueryString(filters, 50, 100)).toBe(
      'limit=50&offset=100&action=build.trigger&objectType=build&result=failure&actor=alice&from=2026-10-01&to=2026-10-31T23%3A59%3A59Z',
    )
  })

  it('percent-encodes actor values and omits empty filters', () => {
    expect(auditQueryString({ actor: 'web api+ops@example.com', action: '' }, 20, 0)).toBe(
      'limit=20&offset=0&actor=web%20api%2Bops%40example.com',
    )
  })
})
