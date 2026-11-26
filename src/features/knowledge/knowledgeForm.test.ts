import { describe, expect, it } from 'vitest'
import { knowledgeError, knowledgeValidation } from './knowledgeForm'
import { ApiError } from '@/api/http'
import { apiErrorText } from '@/api/errors'

describe('knowledgeValidation', () => {
  it('accepts a normal document and empty body', () => {
    expect(knowledgeValidation({ title: '发布手册', body: '', tags: '' })).toBeNull()
    expect(knowledgeValidation({ title: '发布手册', body: '**步骤**', tags: 'ops,runbook' })).toBeNull()
  })

  it('rejects a blank or multi-line title', () => {
    expect(knowledgeValidation({ title: '   ', body: '', tags: '' })).toBe('标题不能为空')
    expect(knowledgeValidation({ title: 'a\nb', body: '', tags: '' })).toBe('标题必须是单行文本')
  })

  it('enforces the UTF-8 byte bounds', () => {
    expect(knowledgeValidation({ title: '汉'.repeat(67), body: '', tags: '' })).toBe('标题最多 200 字节')
    expect(knowledgeValidation({ title: 't', body: '', tags: 'a'.repeat(201) })).toBe('标签最多 200 字节')
    expect(knowledgeValidation({ title: 't', body: 'b'.repeat((100 << 10) + 1), tags: '' })).toBe(
      '正文最多 100 KiB',
    )
  })
})

describe('knowledgeError', () => {
  it('maps validation, not-found and permission failures to Chinese text', () => {
    expect(knowledgeError(new ApiError(400, 'validation_failed', 'x'))).toContain('提交的内容不合法')
    expect(knowledgeError(new ApiError(404, 'not_found', 'x'))).toContain('文档不存在')
    expect(knowledgeError(new ApiError(403, 'forbidden', 'x'))).toContain('只有管理员')
  })

  it('falls back to the shared localized text for other failures', () => {
    expect(knowledgeError(new ApiError(500, 'internal_error', apiErrorText('internal_error')))).toBe(
      '服务器内部错误，请稍后重试',
    )
    expect(knowledgeError(new Error('boom'))).toBe('boom')
    expect(knowledgeError(null)).toBe('保存知识库文档失败，请稍后重试')
  })
})
