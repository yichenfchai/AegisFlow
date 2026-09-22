/**
 * API 门面：页面只从此处导入 api。
 * 当前实现为 mock；接入真实后端时仅替换本文件的实现，
 * 页面代码零改动（符合文档 05 的分层约定）。
 */
import { api as mockApi } from './mock'

export const api = mockApi

export * from './types'
