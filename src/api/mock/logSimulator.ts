/**
 * 构建日志流模拟器（mock 层）：按剧本逐行推送，返回停止函数。
 * 页面通过薄 hook 消费；接真实后端后整段删除，改走 SSE。
 */
import type { LogLine } from '../types'

const FAILED_SCRIPT: [LogLine['stream'], string][] = [
  ['system', '[gopulse] Build #1091 started · trigger: push by Alex Mercer'],
  ['stdout', '$ git checkout a3f8c21'],
  ['stdout', 'HEAD is now at a3f8c21 fix: token parser edge case'],
  ['system', '[stage] Checkout ✓ (8.0s)'],
  ['stdout', '$ pnpm install --frozen-lockfile'],
  ['stdout', 'Lockfile is up to date, resolution step is skipped'],
  ['stdout', 'Done in 33.2s'],
  ['system', '[stage] Install ✓ (41.0s)'],
  ['stdout', '$ pnpm build'],
  ['stdout', 'vite v6.3.5 building for production...'],
  ['stdout', 'transforming (1240) src/parser/token-parser.ts'],
  ['stderr', "src/auth/client.ts:12:34 - error TS2571: Object is of type 'undefined'."],
  ['stderr', '12   const kid = payload.claims[kid];'],
  ['stderr', '                                   ~~~~~~~~~~~~~'],
  ['stderr', 'Failed to compile. Exit code 1.'],
  ['system', '[stage] Build ✗ blocked · Stage 3/4'],
  ['system', '[gopulse] AI 诊断已触发 · LOA 1'],
]

const RUNNING_SCRIPT: [LogLine['stream'], string][] = [
  ['system', '[gopulse] Build #1092 started · trigger: push by Alex Mercer'],
  ['stdout', '$ git checkout 7be01d9'],
  ['system', '[stage] Checkout ✓ (8.0s)'],
  ['stdout', '$ pnpm install --frozen-lockfile'],
  ['stdout', 'Done in 34.1s'],
  ['system', '[stage] Install ✓ (42.0s)'],
  ['stdout', '$ pnpm build'],
  ['stdout', 'vite v6.3.5 building for production...'],
  ['stdout', '✓ 184 modules transformed.'],
  ['stdout', 'dist/index.html                 0.51 kB'],
  ['system', '[stage] Build ▶ running'],
]

export function startLogStream(
  failed: boolean,
  onLine: (line: LogLine) => void,
): () => void {
  const script = failed ? FAILED_SCRIPT : RUNNING_SCRIPT
  let seq = 0
  let i = 0
  let timer: ReturnType<typeof setTimeout>
  const tick = () => {
    if (i >= script.length) return
    const [stream, content] = script[i]
    seq += 1
    onLine({ seq, stream, content, ts: Date.now() })
    i += 1
    timer = setTimeout(tick, failed ? 260 : 320)
  }
  timer = setTimeout(tick, 300)
  return () => clearTimeout(timer)
}
