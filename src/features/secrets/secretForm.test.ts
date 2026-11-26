import { describe, expect, it } from 'vitest'
import { secretCreateValidation, secretError, secretRotateValidation } from './secretForm'
import { ApiError } from '@/api/http'
import { apiErrorText } from '@/api/errors'

describe('secretCreateValidation', () => {
  it('accepts a normal name and matching value', () => {
    expect(
      secretCreateValidation({ name: 'prod-db-password', value: 'super-secret-value-123', confirm: 'super-secret-value-123' }),
    ).toBeNull()
  })

  it('rejects a blank or multi-line name and over-long names', () => {
    expect(secretCreateValidation({ name: '  ', value: 'v', confirm: 'v' })).toBe('名称不能为空')
    expect(secretCreateValidation({ name: 'a\nb', value: 'v', confirm: 'v' })).toBe('名称必须是单行文本')
    expect(secretCreateValidation({ name: 'n'.repeat(129), value: 'v', confirm: 'v' })).toBe('名称最多 128 字节')
  })

  it('rejects empty, over-long and mismatched values', () => {
    expect(secretCreateValidation({ name: 'n', value: '', confirm: '' })).toBe('值不能为空')
    expect(secretCreateValidation({ name: 'n', value: 'a'.repeat(4097), confirm: 'a'.repeat(4097) })).toBe(
      '值最多 4096 字节',
    )
    expect(secretCreateValidation({ name: 'n', value: 'one', confirm: 'two' })).toBe('两次输入的值不一致')
  })
})

describe('secretRotateValidation', () => {
  it('only checks the replacement value and its confirmation', () => {
    expect(secretRotateValidation({ value: 'rotated-value-456', confirm: 'rotated-value-456' })).toBeNull()
    expect(secretRotateValidation({ value: 'rotated', confirm: 'other' })).toBe('两次输入的值不一致')
  })
})

describe('secretError', () => {
  it('explains the unconfigured key store in Chinese instead of a generic dependency error', () => {
    const message = secretError(new ApiError(503, 'secrets_not_configured', 'secrets are not configured'))
    expect(message).toContain('AEGIS_SECRET_KEY')
    expect(message).toContain('无法保存或轮换密钥')
  })

  it('maps conflict, not-found and permission failures', () => {
    expect(secretError(new ApiError(409, 'conflict', 'x'))).toBe('该名称已存在，请换一个名称')
    expect(secretError(new ApiError(404, 'not_found', 'x'))).toBe('密钥不存在或已被删除，请刷新列表')
    expect(secretError(new ApiError(403, 'forbidden', 'x'))).toBe('只有管理员可以管理密钥')
  })

  it('falls back to the shared localized text for other failures', () => {
    expect(secretError(new ApiError(500, 'internal_error', apiErrorText('internal_error')))).toBe(
      '服务器内部错误，请稍后重试',
    )
    expect(secretError(null)).toBe('操作密钥失败，请稍后重试')
  })
})
