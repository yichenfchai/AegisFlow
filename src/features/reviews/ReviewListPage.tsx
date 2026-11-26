import { useState } from 'react'
import { Link, useNavigate } from 'react-router'
import { useQuery } from '@tanstack/react-query'
import {
  Button,
  DataTable,
  Field,
  PageHeader,
  Pagination,
  Select,
  type Column,
} from '@/components/ui/DataTable'
import { StatusBadge } from '@/components/ui/Badge'
import {
  REVIEW_STATUS_TEXT,
  REVIEW_STATUS_TONE,
  reviewsAPI,
  type Review,
  type ReviewStatus,
} from '@/api/reviews'
import { projectsAPI } from '@/api/projects'
import { errorText } from '@/api/errors'
import { fmtRel } from '@/api/format'
import { useSession } from '@/stores/session'
import { ReviewCreateDialog } from './ReviewCreateDialog'

const PAGE_SIZE = 12
const STATUSES: ReviewStatus[] = ['pending', 'approved', 'rejected', 'cancelled']

/**
 * 审查中心（真实接口）：全局可见的审查列表，按项目可见性过滤；项目与状态筛选
 * 和服务端分页。发起审查走同一项目写权限叠加（公开项目需成员、私有项目需维护者），
 * 决策仍由详情页按角色展示并由服务端强制校验。
 */
export function ReviewListPage() {
  const { user } = useSession()
  const canCreate = user?.role === 'admin' || user?.role === 'member'
  const nav = useNavigate()
  const [page, setPage] = useState(1)
  const [status, setStatus] = useState<ReviewStatus | ''>('')
  const [projectFilter, setProjectFilter] = useState('')
  const [creating, setCreating] = useState(false)

  const projectsQuery = useQuery({
    queryKey: ['projects'],
    queryFn: () => projectsAPI.projects(100, 0),
    staleTime: 60_000,
  })
  const projects = projectsQuery.data?.items ?? []

  const { data, isPending, error } = useQuery({
    queryKey: ['reviews', projectFilter, status, page],
    queryFn: () =>
      reviewsAPI.listGlobal({
        projectId: projectFilter || undefined,
        status: status || undefined,
        limit: PAGE_SIZE,
        offset: (page - 1) * PAGE_SIZE,
      }),
  })

  const nameOf = (id: string) => projects.find((project) => project.id === id)?.name ?? id.slice(0, 8)

  const columns: Column<Review>[] = [
    {
      key: 'subject',
      header: '审查',
      render: (review) => (
        <div>
          <Link to={`/reviews/${review.id}`} className="font-medium text-primary hover:underline">
            #{review.number} {review.subject}
          </Link>
          {review.version && <div className="font-mono text-label-sm text-outline">{review.version}</div>}
        </div>
      ),
    },
    { key: 'project', header: '项目', render: (review) => <span title={review.projectId}>{nameOf(review.projectId)}</span> },
    {
      key: 'status',
      header: '状态',
      render: (review) => (
        <StatusBadge tone={REVIEW_STATUS_TONE[review.status]}>{REVIEW_STATUS_TEXT[review.status]}</StatusBadge>
      ),
    },
    {
      key: 'requestedBy',
      header: '发起人',
      render: (review) => (
        <span title={review.requestedBy}>{review.requestedByName || review.requestedBy.slice(0, 8)}</span>
      ),
    },
    {
      key: 'decision',
      header: '决定人 / 理由',
      render: (review) => (
        <div>
          <div className="text-label-sm text-on-surface-variant">
            {review.decidedByName || (review.decidedBy ? review.decidedBy.slice(0, 8) : '—')}
          </div>
          {review.reason && (
            <div className="text-label-sm text-outline break-all" title={review.reason}>
              {review.reason}
            </div>
          )}
        </div>
      ),
    },
    { key: 'createdAt', header: '发起时间', render: (review) => fmtRel(review.createdAt * 1000) },
    {
      key: 'actions',
      header: '操作',
      align: 'right',
      render: (review) => (
        <Link to={`/reviews/${review.id}`} className="text-label-sm text-primary hover:underline">
          详情
        </Link>
      ),
    },
  ]

  return (
    <>
      <PageHeader
        title="审查中心"
        subtitle="变更门禁式审查：发起、批准/拒绝与取消都会写入审计；已批准的审查可用于启用门禁环境的部署"
        actions={
          canCreate ? (
            <Button icon="add" onClick={() => setCreating(true)}>
              发起审查
            </Button>
          ) : undefined
        }
      />
      <div className="mb-space-lg flex flex-wrap items-end gap-space-md">
        <Field label="项目" htmlFor="review-project" className="w-[220px]">
          <Select
            id="review-project"
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
        <Field label="状态" htmlFor="review-status" className="w-[160px]">
          <Select
            id="review-status"
            value={status}
            onChange={(event) => {
              setStatus(event.target.value as ReviewStatus | '')
              setPage(1)
            }}
          >
            <option value="">全部</option>
            {STATUSES.map((value) => (
              <option key={value} value={value}>
                {REVIEW_STATUS_TEXT[value]}
              </option>
            ))}
          </Select>
        </Field>
      </div>
      {error ? (
        <p role="alert" className="text-body-md text-error py-space-lg">
          {errorText(error, '加载审查列表失败')}
        </p>
      ) : isPending ? (
        <p className="text-body-md text-outline py-space-lg">加载中...</p>
      ) : (
        <>
          <DataTable
            columns={columns}
            rows={data?.items ?? []}
            empty={canCreate ? '还没有审查记录；点击右上角发起第一次审查' : '还没有审查记录'}
          />
          <Pagination page={page} pageSize={PAGE_SIZE} total={data?.total ?? 0} onPage={setPage} />
        </>
      )}
      {creating && (
        <ReviewCreateDialog
          projects={projects}
          onClose={() => setCreating(false)}
          onCreated={(review) => {
            setCreating(false)
            nav(`/reviews/${review.id}`, {
              state: { notice: '审查已发起，等待项目维护者或管理员处理' },
            })
          }}
        />
      )}
    </>
  )
}
