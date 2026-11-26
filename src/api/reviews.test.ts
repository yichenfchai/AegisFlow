import { describe, expect, it } from 'vitest'
import {
  REVIEW_STATUS_TEXT,
  reviewListQuery,
  reviewOptionLabel,
} from './reviews'

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
