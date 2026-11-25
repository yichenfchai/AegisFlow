import { ApiError } from '@/api/http'
import { errorText } from '@/api/errors'
import type { ProjectVisibility } from '@/api/projects'

export function byteLength(value: string): number {
  return new TextEncoder().encode(value).length
}

/** 项目创建/编辑表单的字段值（均为原始输入，提交前统一 trim）。 */
export interface ProjectFormValues {
  name: string
  description: string
  repoUrl: string
  defaultBranch: string
  buildCommand: string
  visibility: ProjectVisibility
}

// 与后端 projects.validateInput 保持同一套边界：分支只做轻量 git-ref 形状检查，
// 完整的 ref 校验发生在真正使用它触发构建时。
const branchPattern = /^[A-Za-z0-9][A-Za-z0-9._/-]{0,127}$/
const repoURLPatterns = [
  /^https:\/\/[^\s]+$/,
  /^git@[A-Za-z0-9._-]+:[^\s]+$/,
  /^file:\/\/\/[^\s]*$/,
  /^\/[^\s]*$/,
  /^[A-Za-z]:[\\/][^\s]*$/,
]

/** 表单前置校验：返回首条中文错误，空字符串表示通过。 */
export function projectFormError(values: ProjectFormValues): string {
  const name = values.name
  if (byteLength(name.trim()) < 3 || byteLength(name) > 64) {
    return '项目名称需为 3-64 字节'
  }
  if (name !== name.trim() || /^[.-\s]/.test(name) || /[\\/:?!*"<>|]/.test(name)) {
    return '项目名称不能以点、连字符或空白开头，且不含路径与特殊字符'
  }
  if (byteLength(values.description.trim()) > 512) {
    return '描述最多 512 字节'
  }
  const repoUrl = values.repoUrl.trim()
  if (byteLength(repoUrl) > 512) {
    return '仓库地址最多 512 字节'
  }
  if (repoUrl !== '' && !repoURLPatterns.some((pattern) => pattern.test(repoUrl))) {
    return '仓库地址须为 https:// 或 git@ 地址、file:/// URL，或部署机上的绝对路径'
  }
  const branch = values.defaultBranch.trim()
  if (
    branch !== '' &&
    (!branchPattern.test(branch) ||
      branch.includes('..') ||
      branch.includes('//') ||
      branch.endsWith('/') ||
      branch.endsWith('.lock') ||
      branch.endsWith('.'))
  ) {
    return '默认分支须为普通 git 引用名，例如 main 或 release/2026'
  }
  if (/[\r\n]/.test(values.buildCommand) || byteLength(values.buildCommand) > 512) {
    return '构建命令需为单行，最多 512 字节'
  }
  return ''
}

/** 把项目创建/保存失败的响应映射为中文提示。 */
export function projectWriteError(cause: unknown, action: '创建' | '保存'): string {
  if (cause instanceof ApiError) {
    if (cause.status === 400) {
      return '字段不合法：名称 3-64 字节且不含路径字符；仓库地址须为 https/git@ URL 或绝对路径；分支为普通 git 引用名；构建命令为单行且不超过 512 字节'
    }
    if (cause.status === 403) return `没有${action}权限：私有项目需要是维护者`
    if (cause.status === 404) return '项目不存在或不可见（私有项目需要是成员）'
    if (cause.status === 409) {
      return action === '创建'
        ? '同名项目已存在（不区分大小写）'
        : '同名项目已存在（不区分大小写），或项目已归档（归档后为只读，需先恢复）'
    }
  }
  return errorText(cause, `${action}失败，请稍后重试`)
}
