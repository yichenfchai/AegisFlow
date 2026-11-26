import { ApiError } from '@/api/http'
import { errorText } from '@/api/errors'

/** UTF-8 byte length, matching the server-side bounds. */
export function byteLength(value: string): number {
  return new TextEncoder().encode(value).length
}

const MAX_TITLE_BYTES = 200
const MAX_BODY_BYTES = 100 << 10
const MAX_TAGS_BYTES = 200

export interface KnowledgeFormValues {
  title: string
  body: string
  tags: string
}

/**
 * Client-side mirror of the server document contract. Returns a Chinese
 * message for the first violation, or null when the form is submittable. The
 * server remains authoritative; this only avoids a pointless round trip.
 */
export function knowledgeValidation(values: KnowledgeFormValues): string | null {
  const title = values.title.trim()
  if (title === '') return '标题不能为空'
  if (/[\r\n\t]/.test(title)) return '标题必须是单行文本'
  if (byteLength(title) > MAX_TITLE_BYTES) return `标题最多 ${MAX_TITLE_BYTES} 字节`
  if (/[\r\n\t]/.test(values.tags.trim())) return '标签必须单行、以英文逗号分隔'
  if (byteLength(values.tags.trim()) > MAX_TAGS_BYTES) return `标签最多 ${MAX_TAGS_BYTES} 字节`
  if (byteLength(values.body) > MAX_BODY_BYTES) return '正文最多 100 KiB'
  return null
}

/** Map a write failure to a Chinese, actionable message. */
export function knowledgeError(cause: unknown): string {
  if (cause instanceof ApiError) {
    if (cause.status === 400) return '提交的内容不合法，请检查标题、正文与标签的长度和格式'
    if (cause.status === 404) return '文档不存在或已被删除，请刷新列表'
    if (cause.status === 403) return '只有管理员可以新建、编辑或删除知识库文档'
  }
  return errorText(cause, '保存知识库文档失败，请稍后重试')
}
