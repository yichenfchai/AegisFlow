/** 日志查看器的渲染行类型（真实构建日志经 api/builds.ts 的 LogEvent 转换）。 */
export interface LogLine {
  seq: number
  stream: 'stdout' | 'stderr' | 'system'
  content: string
  ts: number
}
