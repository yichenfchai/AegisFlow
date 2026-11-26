import { Link, useParams } from 'react-router'
import { useQuery } from '@tanstack/react-query'
import { PageHeader } from '@/components/ui/DataTable'
import { StatusBadge } from '@/components/ui/Badge'
import { SCAN_VERDICT_TEXT, SCAN_VERDICT_TONE, scansAPI } from '@/api/scans'
import { ApiError } from '@/api/http'
import { errorText } from '@/api/errors'
import { fmtRel } from '@/api/format'

/**
 * 扫描详情（真实接口）：展示外部工具回写的全部字段与报告外链（新标签打开，
 * rel=noopener noreferrer）；报告地址仅作引用，服务端不会抓取。不可见/不存在统一 404。
 */
export function ScanDetailPage() {
  const { id = '' } = useParams<{ id: string }>()
  const scanQuery = useQuery({
    queryKey: ['scan', id],
    queryFn: () => scansAPI.get(id),
    enabled: id !== '',
    retry: false,
  })
  const scan = scanQuery.data

  if (scanQuery.error && !scan) {
    const status = scanQuery.error instanceof ApiError ? scanQuery.error.status : 0
    return (
      <>
        <PageHeader title="扫描详情" />
        <p role="alert" className="text-body-md text-error py-space-lg">
          {status === 404
            ? '未找到该扫描记录：它可能属于你没有权限查看的项目。'
            : errorText(scanQuery.error, '加载扫描记录失败')}
        </p>
        <Link to="/scans" className="text-body-md text-primary hover:underline">
          返回安全扫描
        </Link>
      </>
    )
  }
  if (scanQuery.isLoading || !scan) {
    return (
      <>
        <PageHeader title="扫描详情" subtitle="加载中..." />
        <p className="text-body-md text-outline py-space-lg">加载中...</p>
      </>
    )
  }

  return (
    <>
      <PageHeader
        title="扫描详情"
        subtitle={`${scan.projectName || scan.projectId.slice(0, 8)} · ${scan.source}`}
        actions={
          <Link to="/scans" className="text-body-md text-primary hover:underline">
            返回安全扫描
          </Link>
        }
      />

      <div className="grid grid-cols-[340px_1fr] gap-gutter">
        <aside className="bg-surface-container rounded-lg border border-surface-container-highest p-space-xl space-y-space-lg self-start">
          <MetaRow label="结论">
            <StatusBadge tone={SCAN_VERDICT_TONE[scan.verdict]}>{SCAN_VERDICT_TEXT[scan.verdict]}</StatusBadge>
          </MetaRow>
          <MetaRow label="项目">
            <span title={scan.projectId}>{scan.projectName || scan.projectId.slice(0, 8)}</span>
          </MetaRow>
          <MetaRow label="来源">
            <span className="break-all">{scan.source}</span>
          </MetaRow>
          <MetaRow label="工具">
            <span className="font-mono">{scan.tool || '—'}</span>
          </MetaRow>
          <MetaRow label="版本">
            <span className="font-mono">{scan.version || '—'}</span>
          </MetaRow>
          <MetaRow label="回写时间">{fmtRel(scan.receivedAt * 1000)}</MetaRow>
        </aside>

        <section className="bg-surface-container rounded-lg border border-surface-container-highest p-space-xl self-start space-y-space-lg">
          <div>
            <h2 className="text-headline-md font-semibold mb-space-md">摘要</h2>
            <p className="text-body-md text-on-surface-variant whitespace-pre-wrap break-all">
              {scan.summary || '（未提供摘要）'}
            </p>
          </div>
          <div>
            <h3 className="text-label-md text-outline mb-space-sm">报告链接</h3>
            {scan.reportUrl ? (
              <div>
                <a
                  href={scan.reportUrl}
                  target="_blank"
                  rel="noopener noreferrer"
                  className="text-body-md text-primary hover:underline"
                >
                  查看外部扫描报告
                </a>
                <div className="font-mono text-label-sm text-outline break-all mt-space-xs">{scan.reportUrl}</div>
                <p className="text-label-sm text-outline mt-space-xs">仅作展示引用；AegisFlow 不会抓取该地址。</p>
              </div>
            ) : (
              <p className="text-body-md text-outline">—</p>
            )}
          </div>
        </section>
      </div>
    </>
  )
}

function MetaRow({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="flex items-start justify-between gap-space-lg text-body-sm">
      <span className="text-outline shrink-0">{label}</span>
      <span className="text-on-surface text-right min-w-0">{children}</span>
    </div>
  )
}
