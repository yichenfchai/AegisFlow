/** Real knowledge-base API: every authenticated role reads; only admins write. */
import { apiRequest } from '@/stores/session'
import type { Page } from '@/api/account'

/**
 * One knowledge document in the list projection. The list endpoint never
 * includes the body; fetch the detail to render it. `createdByName` and
 * `updatedByName` are resolved by the server for display and omitted when the
 * actor account no longer resolves.
 */
export interface KnowledgeDoc {
  id: string
  /** Document title, 1-200 UTF-8 bytes on one line. */
  title: string
  /** Canonical comma-separated tags without spaces after commas; empty when none. */
  tags: string
  /** Author actor id snapshot. */
  createdBy: string
  createdByName?: string
  updatedByName?: string
  /** Unix seconds. */
  createdAt: number
  /** Unix seconds of the last create/update. */
  updatedAt: number
}

/** Detail projection: list metadata plus the stored body. */
export interface KnowledgeDocument extends KnowledgeDoc {
  /**
   * Plain text or Markdown stored verbatim by the server. Render it with the
   * shared markdown renderer (raw HTML disabled, http(s)-only links); never
   * inject it as HTML without escaping.
   */
  body: string
}

/** Create/update payload. The body is stored verbatim; tags are optional. */
export interface KnowledgeInput {
  title: string
  body: string
  tags?: string
}

export interface KnowledgeListQuery {
  q?: string
  limit?: number
  offset?: number
}

/**
 * Query string shared by the knowledge list: paging is always present and the
 * search text is percent-encoded because it is free-form UTF-8.
 */
export function knowledgeListQuery({ q, limit = 20, offset = 0 }: KnowledgeListQuery): string {
  const parts = [`limit=${limit}`, `offset=${offset}`]
  if (q) parts.push(`q=${encodeURIComponent(q)}`)
  return parts.join('&')
}

/**
 * Knowledge API. Reads are available to every authenticated role; create,
 * update and delete answer 403 for member/viewer and are audited server-side.
 */
export const knowledgeAPI = {
  list: (query: KnowledgeListQuery = {}) =>
    apiRequest<Page<KnowledgeDoc>>(`/knowledge?${knowledgeListQuery(query)}`),
  get: (id: string) => apiRequest<KnowledgeDocument>(`/knowledge/${encodeURIComponent(id)}`),
  create: (input: KnowledgeInput) =>
    apiRequest<KnowledgeDocument>('/knowledge', { method: 'POST', body: JSON.stringify(input) }),
  update: (id: string, input: KnowledgeInput) =>
    apiRequest<KnowledgeDocument>(`/knowledge/${encodeURIComponent(id)}`, {
      method: 'PUT',
      body: JSON.stringify(input),
    }),
  remove: (id: string) =>
    apiRequest<{ deleted: boolean }>(`/knowledge/${encodeURIComponent(id)}`, { method: 'DELETE' }),
}

/** Split the canonical comma-separated tag line into display tags. */
export function knowledgeTags(tags: string): string[] {
  return tags
    .split(',')
    .map((tag) => tag.trim())
    .filter(Boolean)
}
