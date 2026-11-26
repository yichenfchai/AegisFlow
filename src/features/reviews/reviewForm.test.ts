import { describe, expect, it } from 'vitest'
import { ApiError } from '@/api/http'
import {
  reviewActionError,
  reviewCreateError,
  reviewCreateValidation,
  reviewReasonValidation,
} from './reviewForm'

const valid = {
  subject: '发布 v1.2.3',
  description: '',
  version: 'v1.2.3',
  externalUrl: 'https://example.com/reviews/1?state=open',
}

describe('reviewCreateValidation', () => {
  it('accepts a well-formed review request', () => {
    expect(reviewCreateValidation(valid)).toBeNull()
  })

  it('requires a 3-200 byte single-line subject', () => {
    expect(reviewCreateValidation({ ...valid, subject: '  a  ' })).toContain('3-200 字节')
    expect(reviewCreateValidation({ ...valid, subject: 'a'.repeat(201) })).toContain('3-200 字节')
    expect(reviewCreateValidation({ ...valid, subject: '第一行\n第二行' })).toContain('单行')
  })

  it('rejects a version over 64 bytes or with control characters', () => {
    expect(reviewCreateValidation({ ...valid, version: 'v'.repeat(65) })).toContain('64 字节')
    expect(reviewCreateValidation({ ...valid, version: 'v1\t2' })).toContain('单行')
  })

  it('limits the description to 2000 bytes', () => {
    expect(reviewCreateValidation({ ...valid, description: 'd'.repeat(2001) })).toContain('2000 字节')
  })

  it('only accepts absolute http(s) links without userinfo or fragment', () => {
    expect(reviewCreateValidation({ ...valid, externalUrl: 'ftp://example.com/a' })).toContain('http(s)')
    expect(reviewCreateValidation({ ...valid, externalUrl: 'https://user:pw@example.com/a' })).toContain('http(s)')
    expect(reviewCreateValidation({ ...valid, externalUrl: 'https://example.com/a#frag' })).toContain('http(s)')
    expect(reviewCreateValidation({ ...valid, externalUrl: '' })).toBeNull()
  })
})

describe('reviewReasonValidation', () => {
  it('requires a reject reason but allows an empty approve reason', () => {
    expect(reviewReasonValidation('   ', true)).toBe('请填写拒绝理由')
    expect(reviewReasonValidation('   ', false)).toBeNull()
  })

  it('keeps the reason single-line and within 512 bytes', () => {
    expect(reviewReasonValidation('第一行\n第二行', true)).toContain('单行')
    expect(reviewReasonValidation('r'.repeat(513), true)).toContain('512 字节')
  })
})

describe('review error mapping', () => {
  it('explains write authorization and terminal-state conflicts', () => {
    expect(reviewCreateError(new ApiError(403, 'forbidden', 'x'))).toContain('没有发起审查的权限')
    expect(reviewActionError(new ApiError(409, 'conflict', 'x'), '批准')).toContain('无法批准')
    expect(reviewActionError(new ApiError(403, 'forbidden', 'x'), '取消')).toContain('只有审查发起人')
  })

  it('falls back to the unified Chinese error text', () => {
    expect(reviewCreateError(new Error('网络中断'))).toBe('网络中断')
  })
})
