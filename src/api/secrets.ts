/**
 * Real independent secret-store API (admin-only). Values are write-only: no
 * response type here has a value or ciphertext field, and the server never
 * returns one. The page can create a secret, rotate its value and delete it,
 * but can never read a stored value back.
 */
import { apiRequest } from '@/stores/session'
import type { Page } from '@/api/account'

/**
 * Public metadata of one secret: identity and lifecycle only. There is
 * deliberately no value field, so a component cannot accidentally render one.
 */
export interface SecretMetadata {
  id: string
  /** Unique human reference, 1-128 UTF-8 bytes on one line. */
  name: string
  /** Creator actor id snapshot. */
  createdBy: string
  createdByName?: string
  /** Unix seconds. */
  createdAt: number
  /** Unix seconds of the last value rotation; equal to createdAt until rotated. */
  updatedAt: number
}

/** Create payload: the value is sealed at rest and never returned. */
export interface SecretCreateInput {
  name: string
  value: string
}

/** Rotation payload: only the new value is sent. */
export interface SecretRotateInput {
  value: string
}

/**
 * Secret store API. `list` returns metadata only; `create`/`rotate` answer 503
 * with code `secrets_not_configured` when the server has no AEGIS_SECRET_KEY.
 */
export const secretsAPI = {
  list: (limit = 20, offset = 0) =>
    apiRequest<Page<SecretMetadata>>(`/secrets?limit=${limit}&offset=${offset}`),
  create: (input: SecretCreateInput) =>
    apiRequest<SecretMetadata>('/secrets', { method: 'POST', body: JSON.stringify(input) }),
  rotate: (id: string, input: SecretRotateInput) =>
    apiRequest<SecretMetadata>(`/secrets/${encodeURIComponent(id)}`, {
      method: 'PUT',
      body: JSON.stringify(input),
    }),
  remove: (id: string) =>
    apiRequest<{ deleted: boolean }>(`/secrets/${encodeURIComponent(id)}`, { method: 'DELETE' }),
}
