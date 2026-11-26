import { beforeEach, describe, expect, it, vi } from 'vitest'
import { knowledgeAPI, knowledgeListQuery, knowledgeTags } from './knowledge'
import { apiRequest } from '@/stores/session'
import { ApiError } from './http'

vi.mock('@/stores/session', () => ({ apiRequest: vi.fn() }))

const apiRequestMock = vi.mocked(apiRequest)

beforeEach(() => {
  apiRequestMock.mockReset()
})

describe('knowledgeListQuery', () => {
  it('always carries server-side paging and omits an empty search', () => {
    expect(knowledgeListQuery({})).toBe('limit=20&offset=0')
    expect(knowledgeListQuery({ q: '', limit: 100, offset: 40 })).toBe('limit=100&offset=40')
  })

  it('percent-encodes the free-form search text', () => {
    expect(knowledgeListQuery({ q: '发布 手册&x=1', limit: 12, offset: 0 })).toBe(
      'limit=12&offset=0&q=%E5%8F%91%E5%B8%83%20%E6%89%8B%E5%86%8C%26x%3D1',
    )
  })
})

describe('knowledgeTags', () => {
  it('splits the canonical tag line and drops blanks', () => {
    expect(knowledgeTags('ops,runbook,security')).toEqual(['ops', 'runbook', 'security'])
    expect(knowledgeTags('')).toEqual([])
    expect(knowledgeTags(' ops , , security ')).toEqual(['ops', 'security'])
  })
})

const doc = {
  id: 'doc-1',
  title: '发布手册',
  tags: 'ops,runbook',
  createdBy: 'user-1',
  createdByName: 'admin',
  updatedByName: 'admin',
  createdAt: 1_790_000_000,
  updatedAt: 1_790_000_100,
  body: '# 发布手册\n\n**先备份**',
}

describe('knowledgeAPI', () => {
  it('requests the encoded knowledge page and passes the metadata through', async () => {
    const page = { items: [doc], total: 1, limit: 12, offset: 0 }
    apiRequestMock.mockResolvedValueOnce(page)
    await expect(knowledgeAPI.list({ q: '发布', limit: 12, offset: 0 })).resolves.toEqual(page)
    expect(apiRequestMock).toHaveBeenCalledWith(
      '/knowledge?limit=12&offset=0&q=%E5%8F%91%E5%B8%83',
    )
  })

  it('requests the encoded detail id and returns the body', async () => {
    apiRequestMock.mockResolvedValueOnce(doc)
    await expect(knowledgeAPI.get('doc/1')).resolves.toEqual(doc)
    expect(apiRequestMock).toHaveBeenCalledWith('/knowledge/doc%2F1')
  })

  it('creates through POST with the exact JSON input', async () => {
    apiRequestMock.mockResolvedValueOnce(doc)
    await knowledgeAPI.create({ title: '发布手册', body: '**先备份**', tags: 'ops' })
    expect(apiRequestMock).toHaveBeenCalledWith('/knowledge', {
      method: 'POST',
      body: JSON.stringify({ title: '发布手册', body: '**先备份**', tags: 'ops' }),
    })
  })

  it('updates through PUT on the encoded id and removes through DELETE', async () => {
    apiRequestMock.mockResolvedValueOnce(doc)
    await knowledgeAPI.update('doc/1', { title: 'v2', body: 'x' })
    expect(apiRequestMock).toHaveBeenCalledWith('/knowledge/doc%2F1', {
      method: 'PUT',
      body: JSON.stringify({ title: 'v2', body: 'x' }),
    })
    apiRequestMock.mockResolvedValueOnce({ deleted: true })
    await expect(knowledgeAPI.remove('doc/1')).resolves.toEqual({ deleted: true })
    expect(apiRequestMock).toHaveBeenCalledWith('/knowledge/doc%2F1', { method: 'DELETE' })
  })

  it('propagates the localized ApiError so the page can render shared Chinese text', async () => {
    const failure = new ApiError(403, 'forbidden', '当前账号没有执行该操作的权限')
    apiRequestMock.mockRejectedValueOnce(failure)
    await expect(knowledgeAPI.create({ title: 't', body: 'b' })).rejects.toBe(failure)
  })
})
