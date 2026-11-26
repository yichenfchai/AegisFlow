import { useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import {
  Button,
  ConfirmDialog,
  DataTable,
  Field,
  PageHeader,
  Pagination,
  Select,
  type Column,
} from '@/components/ui/DataTable'
import { StatusBadge } from '@/components/ui/Badge'
import {
  SECURITY_EVENT_STATUS_TEXT,
  SECURITY_EVENT_STATUS_TONE,
  SECURITY_SEVERITY_TEXT,
  SECURITY_SEVERITY_TONE,
  securityEventsAPI,
  type SecurityEvent,
  type SecurityEventStatus,
  type SecuritySeverity,
} from '@/api/securityEvents'
import { errorText } from '@/api/errors'
import { fmtRel } from '@/api/format'

const PAGE_SIZE = 12
const SEVERITIES: SecuritySeverity[] = ['low', 'medium', 'high', 'critical']
const STATUSES: SecurityEventStatus[] = ['open', 'acknowledged']

/**
 * 攻击防御（真实接口，admin-only）：外部检测工具通过签名接口回写的事件；管理员可
 * 二次确认后把开放事件置为已确认（服务端同事务写审计，重复确认幂等）。筛选与分页在
 * 服务端生效；确认按钮只是交互镜像，路由与服务端仍强制管理员角色。
 */
export function DefenseListPage() {
  const queryClient = useQueryClient()
  const [page, setPage] = useState(1)
  const [severity, setSeverity] = useState<SecuritySeverity | ''>('')
  const [status, setStatus] = useState<SecurityEventStatus | ''>('')
  const [ackTarget, setAckTarget] = useState<SecurityEvent | null>(null)
  const [ackError, setAckError] = useState('')
  const [notice, setNotice] = useState('')
  const [busy, setBusy] = useState(false)

  const { data, isPending, error } = useQuery({
    queryKey: ['security-events', severity, status, page],
    queryFn: () =>
      securityEventsAPI.list({
        severity: severity || undefined,
        status: status || undefined,
        limit: PAGE_SIZE,
        offset: (page - 1) * PAGE_SIZE,
      }),
  })

  const ack = async () => {
    const target = ackTarget
    if (!target || busy) return
    setAckError('')
    setBusy(true)
    try {
      const updated = await securityEventsAPI.ack(target.id)
      setAckTarget(null)
      setNotice(`已确认事件 ${updated.eventType}（${updated.source}）`)
      await queryClient.invalidateQueries({ queryKey: ['security-events'] })
    } catch (cause) {
      setAckError(errorText(cause, '确认事件失败，请稍后重试'))
    } finally {
      setBusy(false)
    }
  }

  const columns: Column<SecurityEvent>[] = [
    {
      key: 'severity',
      header: '严重度',
      render: (event) => (
        <StatusBadge tone={SECURITY_SEVERITY_TONE[event.severity]}>
          {SECURITY_SEVERITY_TEXT[event.severity]}
        </StatusBadge>
      ),
    },
    {
      key: 'eventType',
      header: '事件类型',
      render: (event) => <span className="break-all font-mono text-body-sm">{event.eventType}</span>,
    },
    { key: 'source', header: '来源', render: (event) => <span className="break-all">{event.source}</span> },
    {
      key: 'summary',
      header: '摘要',
      render: (event) => (
        <span className="line-clamp-2 max-w-[360px] break-all text-on-surface-variant" title={event.summary}>
          {event.summary}
        </span>
      ),
    },
    { key: 'occurredAt', header: '发生时间', render: (event) => fmtRel(event.occurredAt * 1000) },
    {
      key: 'status',
      header: '状态',
      render: (event) => (
        <StatusBadge tone={SECURITY_EVENT_STATUS_TONE[event.status]}>
          {SECURITY_EVENT_STATUS_TEXT[event.status]}
        </StatusBadge>
      ),
    },
    {
      key: 'acknowledgedBy',
      header: '确认人 / 时间',
      render: (event) =>
        event.acknowledgedAt ? (
          <div>
            <div title={event.acknowledgedBy}>{event.acknowledgedByName || event.acknowledgedBy?.slice(0, 8) || '—'}</div>
            <div className="text-label-sm text-outline">{new Date(event.acknowledgedAt * 1000).toLocaleString()}</div>
          </div>
        ) : (
          <span className="text-label-sm text-outline">—</span>
        ),
    },
    {
      key: 'actions',
      header: '操作',
      align: 'right',
      render: (event) =>
        event.status === 'open' ? (
          <Button
            variant="secondary"
            size="sm"
            disabled={busy}
            onClick={() => {
              setAckError('')
              setAckTarget(event)
            }}
          >
            确认
          </Button>
        ) : (
          <span className="text-label-sm text-outline">已确认</span>
        ),
    },
  ]

  return (
    <>
      <PageHeader
        title="攻击防御"
        subtitle="外部检测工具通过签名接口回写的防御事件；管理员确认后记录确认人与时间，并写入审计"
      />
      <div className="mb-space-lg flex flex-wrap items-end gap-space-md">
        <Field label="严重度" htmlFor="event-severity" className="w-[160px]">
          <Select
            id="event-severity"
            value={severity}
            onChange={(event) => {
              setSeverity(event.target.value as SecuritySeverity | '')
              setPage(1)
            }}
          >
            <option value="">全部</option>
            {SEVERITIES.map((value) => (
              <option key={value} value={value}>
                {SECURITY_SEVERITY_TEXT[value]}
              </option>
            ))}
          </Select>
        </Field>
        <Field label="状态" htmlFor="event-status" className="w-[160px]">
          <Select
            id="event-status"
            value={status}
            onChange={(event) => {
              setStatus(event.target.value as SecurityEventStatus | '')
              setPage(1)
            }}
          >
            <option value="">全部</option>
            {STATUSES.map((value) => (
              <option key={value} value={value}>
                {SECURITY_EVENT_STATUS_TEXT[value]}
              </option>
            ))}
          </Select>
        </Field>
      </div>
      {notice && (
        <p role="status" className="mb-space-md text-body-sm text-success bg-success/10 rounded px-space-lg py-space-md">
          {notice}
        </p>
      )}
      {ackError && (
        <p role="alert" className="mb-space-md text-body-sm text-error bg-error/10 rounded px-space-lg py-space-md">
          {ackError}
        </p>
      )}
      {error ? (
        <p role="alert" className="text-body-md text-error py-space-lg">
          {errorText(error, '加载防御事件失败')}
        </p>
      ) : isPending ? (
        <p className="text-body-md text-outline py-space-lg">加载中...</p>
      ) : (
        <>
          <DataTable
            columns={columns}
            rows={data?.items ?? []}
            empty="还没有防御事件；外部检测工具通过签名接口回写后才会出现在这里"
          />
          <Pagination page={page} pageSize={PAGE_SIZE} total={data?.total ?? 0} onPage={setPage} />
        </>
      )}

      <ConfirmDialog
        open={ackTarget !== null}
        title="确认防御事件"
        confirmText="确认事件"
        onConfirm={() => void ack()}
        onCancel={() => setAckTarget(null)}
      >
        <p className="mb-space-md">确认后事件标记为「已确认」并记录确认人与时间，同时写入审计；该操作不可撤销。</p>
        {ackTarget && (
          <p className="text-body-sm text-on-surface">
            {ackTarget.source} · {SECURITY_SEVERITY_TEXT[ackTarget.severity]} · {ackTarget.eventType}
          </p>
        )}
        {ackError && (
          <p role="alert" className="mt-space-md text-body-sm text-error">
            {ackError}
          </p>
        )}
      </ConfirmDialog>
    </>
  )
}
