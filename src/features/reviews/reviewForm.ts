/** 审查表单的校验与错误文案：与服务端契约一致的中文提示，错误码仍走 errors.ts 映射。 */
import { ApiError } from '@/api/http'
import { errorText } from '@/api/errors'

const MIN_SUBJECT_BYTES = 3
const MAX_SUBJECT_BYTES = 200
const MAX_DESCRIPTION_BYTES = 2000
const MAX_VERSION_BYTES = 64
const MAX_URL_BYTES = 2048
const MAX_REASON_BYTES = 512

/** 与后端一致的 UTF-8 字节长度（服务端按字节限长，不按字符数）。 */
export function byteLength(value: string): number {
  return new TextEncoder().encode(value).length
}

/** 后端要求单行字段不含控制字符（含 CR/LF/TAB）。 */
function hasControlCharacter(value: string): boolean {
  return /\p{Cc}/u.test(value)
}

export interface ReviewFormValues {
  subject: string
  description: string
  version: string
  externalUrl: string
}

/**
 * 发起审查的前端校验：返回第一条中文错误；通过返回 null。
 * 服务端仍会独立校验，这里只是避免明显无效的请求。
 */
export function reviewCreateValidation(values: ReviewFormValues): string | null {
  const subject = values.subject.trim()
  const subjectBytes = byteLength(subject)
  if (subjectBytes < MIN_SUBJECT_BYTES || subjectBytes > MAX_SUBJECT_BYTES) {
    return `审查主题需为 3-200 字节（当前 ${subjectBytes} 字节）`
  }
  if (hasControlCharacter(subject)) return '审查主题需为单行，不能包含换行或控制字符'
  if (byteLength(values.description) > MAX_DESCRIPTION_BYTES) return '审查描述最多 2000 字节'
  const version = values.version.trim()
  if (byteLength(version) > MAX_VERSION_BYTES) return '版本最多 64 字节'
  if (hasControlCharacter(version)) return '版本需为单行，不能包含换行或控制字符'
  const externalUrl = values.externalUrl.trim()
  if (externalUrl !== '' && !validExternalUrl(externalUrl)) {
    return '外部链接需为 http(s) 绝对地址（不含账号信息、片段或空白字符），最多 2048 字节'
  }
  return null
}

/** 批准理由选填、拒绝理由必填；两者都必须是单行且不超过 512 字节。 */
export function reviewReasonValidation(reason: string, required: boolean): string | null {
  const trimmed = reason.trim()
  if (required && trimmed === '') return '请填写拒绝理由'
  if (byteLength(trimmed) > MAX_REASON_BYTES) return '理由最多 512 字节'
  if (hasControlCharacter(reason)) return '理由需为单行，不能包含换行或控制字符'
  return null
}

/** 外部链接只展示、不抓取；与服务端一致地拒绝 userinfo 与 fragment。 */
function validExternalUrl(value: string): boolean {
  if (byteLength(value) > MAX_URL_BYTES) return false
  if (/\s/u.test(value) || hasControlCharacter(value)) return false
  try {
    const parsed = new URL(value)
    return (
      (parsed.protocol === 'http:' || parsed.protocol === 'https:') &&
      parsed.hostname !== '' &&
      parsed.username === '' &&
      parsed.password === '' &&
      parsed.hash === ''
    )
  } catch {
    return false
  }
}

/** 发起审查失败 → 中文文案；错误码兜底走 errors.ts。 */
export function reviewCreateError(cause: unknown): string {
  if (cause instanceof ApiError) {
    if (cause.status === 400) {
      return '请求不合法：审查主题需 3-200 字节单行，描述最多 2000 字节，版本最多 64 字节单行，外部链接须为 http(s) 地址'
    }
    if (cause.status === 403) return '你在该项目没有发起审查的权限：公开项目需要成员角色，私有项目需要维护者'
    if (cause.status === 404) return '项目不存在、对你不可见，或你在该项目没有写权限'
  }
  return errorText(cause, '发起审查失败，请稍后重试')
}

/** 批准 / 拒绝 / 取消失败 → 中文文案。 */
export function reviewActionError(cause: unknown, action: '批准' | '拒绝' | '取消'): string {
  if (cause instanceof ApiError) {
    if (cause.status === 400) {
      if (action === '取消') return '请求不合法，请刷新后重试'
      return action === '拒绝'
        ? '拒绝理由不合法：需为单行且最多 512 字节'
        : `${action}理由不合法：需为单行且最多 512 字节`
    }
    if (cause.status === 403) {
      return action === '取消'
        ? '只有审查发起人、项目维护者或管理员可以取消该审查'
        : '只有项目维护者或管理员可以批准或拒绝该审查'
    }
    if (cause.status === 404) return '审查不存在，或它所属的项目对你不可见'
    if (cause.status === 409) return `无法${action}：审查已被处理，仅待审查的记录可以${action}`
  }
  return errorText(cause, `${action}失败，请稍后重试`)
}
