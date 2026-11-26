import { beforeEach, describe, expect, it, vi } from 'vitest'
import { secretsAPI } from './secrets'
import { apiRequest } from '@/stores/session'
import { ApiError } from './http'

vi.mock('@/stores/session', () => ({ apiRequest: vi.fn() }))

const apiRequestMock = vi.mocked(apiRequest)

beforeEach(() => {
  apiRequestMock.mockReset()
})

const metadata = {
  id: 'secret-1',
  name: 'prod-db-password',
  createdBy: 'user-1',
  createdByName: 'admin',
  createdAt: 1_790_000_000,
  updatedAt: 1_790_000_000,
}

describe('secretsAPI', () => {
  it('lists metadata with server-side paging only', async () => {
    const page = { items: [metadata], total: 1, limit: 20, offset: 0 }
    apiRequestMock.mockResolvedValueOnce(page)
    await expect(secretsAPI.list(20, 0)).resolves.toEqual(page)
    expect(apiRequestMock).toHaveBeenCalledWith('/secrets?limit=20&offset=0')
  })

  it('creates through POST and sends the value only in the request body', async () => {
    apiRequestMock.mockResolvedValueOnce(metadata)
    await secretsAPI.create({ name: 'prod-db-password', value: 'super-secret-value-123' })
    expect(apiRequestMock).toHaveBeenCalledWith('/secrets', {
      method: 'POST',
      body: JSON.stringify({ name: 'prod-db-password', value: 'super-secret-value-123' }),
    })
  })

  it('rotates through PUT on the encoded id and never puts the value in the path', async () => {
    apiRequestMock.mockResolvedValueOnce(metadata)
    await secretsAPI.rotate('secret/1', { value: 'rotated-value-456' })
    expect(apiRequestMock).toHaveBeenCalledWith('/secrets/secret%2F1', {
      method: 'PUT',
      body: JSON.stringify({ value: 'rotated-value-456' }),
    })
  })

  it('removes through DELETE on the encoded id', async () => {
    apiRequestMock.mockResolvedValueOnce({ deleted: true })
    await expect(secretsAPI.remove('secret/1')).resolves.toEqual({ deleted: true })
    expect(apiRequestMock).toHaveBeenCalledWith('/secrets/secret%2F1', { method: 'DELETE' })
  })

  it('propagates the unconfigured-store ApiError without hiding its message', async () => {
    const failure = new ApiError(503, 'secrets_not_configured', '密钥存储未配置：请在服务端设置 AEGIS_SECRET_KEY 后重启。')
    apiRequestMock.mockRejectedValueOnce(failure)
    await expect(secretsAPI.create({ name: 'x', value: 'y' })).rejects.toBe(failure)
  })

  it('exposes metadata without any value or ciphertext field', () => {
    expect(Object.keys(metadata)).not.toContain('value')
    expect(Object.keys(metadata)).not.toContain('ciphertext')
  })
})
