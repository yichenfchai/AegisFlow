import { ApiError } from '@/api/http'
import { errorText } from '@/api/errors'

/** UTF-8 byte length, matching the server-side bounds. */
export function byteLength(value: string): number {
  return new TextEncoder().encode(value).length
}

const MAX_NAME_BYTES = 128
const MAX_VALUE_BYTES = 4096

export interface SecretCreateValues {
  name: string
  value: string
  confirm: string
}

export interface SecretRotateValues {
  value: string
  confirm: string
}

/** Shared value/confirmation rules: the form asks twice because a typo is unrecoverable. */
function valueValidation(value: string, confirm: string): string | null {
  if (value === '') return '值不能为空'
  if (byteLength(value) > MAX_VALUE_BYTES) return `值最多 ${MAX_VALUE_BYTES} 字节`
  if (value !== confirm) return '两次输入的值不一致'
  return null
}

/**
 * Client-side mirror of the secret create contract. The value is write-only, so
 * a typo cannot be corrected by reading it back: the form requires confirmation.
 */
export function secretCreateValidation({ name, value, confirm }: SecretCreateValues): string | null {
  const trimmed = name.trim()
  if (trimmed === '') return '名称不能为空'
  if (/[\r\n\t]/.test(trimmed)) return '名称必须是单行文本'
  if (byteLength(trimmed) > MAX_NAME_BYTES) return `名称最多 ${MAX_NAME_BYTES} 字节`
  return valueValidation(value, confirm)
}

/** Validation for rotating an existing secret's value. */
export function secretRotateValidation({ value, confirm }: SecretRotateValues): string | null {
  return valueValidation(value, confirm)
}

/**
 * Map a secret-store failure to a Chinese, actionable message. The most
 * important case is the unconfigured server (503 secrets_not_configured): the
 * page must explain that writes are disabled until AEGIS_SECRET_KEY is set,
 * not show a generic dependency error.
 */
export function secretError(cause: unknown): string {
  if (cause instanceof ApiError) {
    if (cause.code === 'secrets_not_configured') {
      return '服务端未配置密钥存储（AEGIS_SECRET_KEY），无法保存或轮换密钥；请联系管理员配置后重启服务。'
    }
    if (cause.status === 409) return '该名称已存在，请换一个名称'
    if (cause.status === 404) return '密钥不存在或已被删除，请刷新列表'
    if (cause.status === 403) return '只有管理员可以管理密钥'
    if (cause.status === 400) return '提交的内容不合法，请检查名称与值的长度'
  }
  return errorText(cause, '操作密钥失败，请稍后重试')
}
