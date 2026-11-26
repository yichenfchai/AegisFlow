import { useState } from 'react'
import { Link } from 'react-router'
import { useQuery } from '@tanstack/react-query'
import { DataTable, Field, PageHeader, Pagination, Select, type Column } from '@/components/ui/DataTable'
import { StatusBadge } from '@/components/ui/Badge'
import {
  SCAN_VERDICT_TEXT,
  SCAN_VERDICT_TONE,
  scansAPI,
  type Scan,
  type ScanVerdict,
} from '@/api/scans'
import { projectsAPI } from '@/api/projects'
import { errorText } from '@/api/errors'
import { fmtRel } from '@/api/format'

const PAGE_SIZE = 12
const VERDICTS: ScanVerdict[] = ['passed', 'failed', 'warning']

/**
 * 安全扫描（真实接口）：外部扫描工具通过签名接口回写的结果登记表，AegisFlow 不自研
 * 扫描引擎；列表按项目可见性过滤，项目与 verdict 筛选、服务端分页。扫描结果只读。
 */
export function ScanListPage() {
  const [page, setPage] = useState(1)
  const [verdict, setVerdict] = useState<ScanVerdict | ''>('')
  const [projectFilter, setProjectFilter] = useState('')

  const projectsQuery = useQuery({
    queryKey: ['projects'],
    queryFn: () => projectsAPI.projects(100, 0),
    staleTime: 60_000,
  })
  const projects = projectsQuery.data?.items ?? []

  const { data, isPending, error } = useQuery({
    queryKey: ['scans', projectFilter, verdict, page],
    queryFn: () =>
      scansAPI.listGlobal({
        projectId: projectFilter || undefined,
        verdict: verdict || undefined,
        limit: PAGE_SIZE,
        offset: (page - 1) * PAGE_SIZE,
      }),
  })

  const columns: Column<Scan>[] = [
    {
      key: 'project',
      header: '项目',
      render: (scan) => (
        <span title={scan.projectId} className="font-medium text-on-surface">
          {scan.projectName || scan.projectId.slice(0, 8)}
        </span>
      ),
    },
    {
      key: 'source',
      header: '来源 / 工具',
      render: (scan) => (
        <div>
          <div className="break-all">{scan.source}</div>
          {(scan.tool || scan.version) && (
            <div className="font-mono text-label-sm text-outline">
              {[scan.tool, scan.version].filter(Boolean).join(' · ')}
            </div>
          )}
        </div>
      ),
    },
    {
      key: 'verdict',
      header: '结论',
      render: (scan) => (
        <StatusBadge tone={SCAN_VERDICT_TONE[scan.verdict]}>{SCAN_VERDICT_TEXT[scan.verdict]}</StatusBadge>
      ),
    },
    {
      key: 'summary',
      header: '摘要',
      render: (scan) =>
        scan.summary ? (
          <span className="line-clamp-2 max-w-[360px] break-all text-on-surface-variant" title={scan.summary}>
            {scan.summary}
          </span>
        ) : (
          <span className="text-outline">—</span>
        ),
    },
    { key: 'receivedAt', header: '回写时间', render: (scan) => fmtRel(scan.receivedAt * 1000) },
    {
      key: 'actions',
      header: '操作',
      align: 'right',
      render: (scan) => (
        <Link to={`/scans/${scan.id}`} className="text-label-sm text-primary hover:underline">
          详情
        </Link>
      ),
    },
  ]

  return (
    <>
      <PageHeader
        title="安全扫描"
        subtitle="外部扫描工具通过签名接口回写的结果；AegisFlow 只做登记、审计与展示，不自行执行扫描"
      />
      <div className="mb-space-lg flex flex-wrap items-end gap-space-md">
        <Field label="项目" htmlFor="scan-project" className="w-[220px]">
          <Select
            id="scan-project"
            value={projectFilter}
            onChange={(event) => {
              setProjectFilter(event.target.value)
              setPage(1)
            }}
          >
            <option value="">全部项目</option>
            {projects.map((project) => (
              <option key={project.id} value={project.id}>
                {project.name}
              </option>
            ))}
          </Select>
        </Field>
        <Field label="结论" htmlFor="scan-verdict" className="w-[160px]">
          <Select
            id="scan-verdict"
            value={verdict}
            onChange={(event) => {
              setVerdict(event.target.value as ScanVerdict | '')
              setPage(1)
            }}
          >
            <option value="">全部</option>
            {VERDICTS.map((value) => (
              <option key={value} value={value}>
                {SCAN_VERDICT_TEXT[value]}
              </option>
            ))}
          </Select>
        </Field>
      </div>
      {error ? (
        <p role="alert" className="text-body-md text-error py-space-lg">
          {errorText(error, '加载扫描结果失败')}
        </p>
      ) : isPending ? (
        <p className="text-body-md text-outline py-space-lg">加载中...</p>
      ) : (
        <>
          <DataTable
            columns={columns}
            rows={data?.items ?? []}
            empty="还没有扫描结果；外部工具通过签名接口回写后才会出现在这里"
          />
          <Pagination page={page} pageSize={PAGE_SIZE} total={data?.total ?? 0} onPage={setPage} />
        </>
      )}
    </>
  )
}
