import { useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import {
  Button,
  ConfirmDialog,
  DataTable,
  PageHeader,
  Pagination,
  type Column,
} from '@/components/ui/DataTable'
import { secretsAPI, type SecretMetadata } from '@/api/secrets'
import { errorText } from '@/api/errors'
import { fmtRel } from '@/api/format'
import { SecretDialog } from './SecretDialog'
import { secretError } from './secretForm'

const PAGE_SIZE = 12

/**
 * 独立密钥管理页（真实接口，admin-only）：只展示名称、创建人与时间等元数据，
 * 绝不展示值。新建与轮换都是覆盖写，保存后值不可再查看；删除需二次确认。服务端
 * 使用 AES-256-GCM 加密存储，未配置 AEGIS_SECRET_KEY 时保存/轮换返回 503，页面
 * 给出中文提示。导航与路由守卫只是交互层，接口本身仍强制管理员角色。
 */
export function SecretsPage() {
  const queryClient = useQueryClient()
  const [page, setPage] = useState(1)
  const [creating, setCreating] = useState(false)
  const [rotating, setRotating] = useState<SecretMetadata | null>(null)
  const [deleting, setDeleting] = useState<SecretMetadata | null>(null)
  const [notice, setNotice] = useState('')
  const [actionError, setActionError] = useState('')
  const [busy, setBusy] = useState(false)

  const { data, isPending, error } = useQuery({
    queryKey: ['secrets', page],
    queryFn: () => secretsAPI.list(PAGE_SIZE, (page - 1) * PAGE_SIZE),
  })
  const secrets = data?.items ?? []
  const refresh = () => queryClient.invalidateQueries({ queryKey: ['secrets'] })

  const remove = async () => {
    const target = deleting
    if (!target || busy) return
    setActionError('')
    setBusy(true)
    try {
      await secretsAPI.remove(target.id)
      setDeleting(null)
      setNotice(`已删除密钥「${target.name}」，其密文已不可恢复。`)
      await refresh()
    } catch (cause) {
      setActionError(secretError(cause))
    } finally {
      setBusy(false)
    }
  }

  const columns: Column<SecretMetadata>[] = [
    {
      key: 'name',
      header: '名称',
      render: (secret) => <span className="break-all font-medium text-on-surface">{secret.name}</span>,
    },
    {
      key: 'createdByName',
      header: '创建人',
      render: (secret) => <span title={secret.createdBy}>{secret.createdByName || secret.createdBy.slice(0, 8)}</span>,
    },
    {
      key: 'createdAt',
      header: '创建时间',
      render: (secret) => (
        <span title={new Date(secret.createdAt * 1000).toLocaleString()}>{fmtRel(secret.createdAt * 1000)}</span>
      ),
    },
    {
      key: 'updatedAt',
      header: '最近轮换',
      render: (secret) =>
        secret.updatedAt === secret.createdAt ? (
          <span className="text-label-sm text-outline">未轮换</span>
        ) : (
          <span title={new Date(secret.updatedAt * 1000).toLocaleString()}>{fmtRel(secret.updatedAt * 1000)}</span>
        ),
    },
    {
      key: 'actions',
      header: '操作',
      align: 'right',
      render: (secret) => (
        <div className="flex justify-end gap-space-md">
          <Button
            variant="secondary"
            size="sm"
            disabled={busy}
            onClick={() => {
              setActionError('')
              setNotice('')
              setRotating(secret)
            }}
          >
            轮换值
          </Button>
          <Button
            variant="danger"
            size="sm"
            disabled={busy}
            onClick={() => {
              setActionError('')
              setDeleting(secret)
            }}
          >
            删除
          </Button>
        </div>
      ),
    },
  ]

  return (
    <>
      <PageHeader
        title="密钥管理"
        subtitle="值以 AES-256-GCM 加密存储、只写不读；页面只展示名称与元数据，管理员可新建、轮换与删除"
        actions={
          <Button icon="key" onClick={() => { setActionError(''); setCreating(true) }}>
            新建密钥
          </Button>
        }
      />

      <div className="mb-space-lg rounded-lg border border-surface-container-highest bg-surface-container p-space-lg text-body-sm text-on-surface-variant">
        <p>
          安全边界：值保存后无法再次查看，也不能通过任何接口读取；轮换会立即用新值替换密文。服务端未配置
          <span className="font-mono"> AEGIS_SECRET_KEY </span>
          时，保存与轮换会被拒绝（503），列表与删除不受影响。
        </p>
      </div>

      {notice && (
        <p role="status" className="mb-space-md text-body-sm text-success bg-success/10 rounded px-space-lg py-space-md">
          {notice}
        </p>
      )}
      {actionError && (
        <p role="alert" className="mb-space-md text-body-sm text-error bg-error/10 rounded px-space-lg py-space-md">
          {actionError}
        </p>
      )}

      {error ? (
        <p role="alert" className="text-body-md text-error py-space-lg">
          {errorText(error, '加载密钥列表失败')}
        </p>
      ) : isPending ? (
        <p className="text-body-md text-outline py-space-lg">加载中...</p>
      ) : (
        <>
          <DataTable
            columns={columns}
            rows={secrets}
            empty="还没有保存任何密钥；新建后只能看到名称与元数据，值不会再次显示"
          />
          <Pagination page={page} pageSize={PAGE_SIZE} total={data?.total ?? 0} onPage={setPage} />
        </>
      )}

      {creating && (
        <SecretDialog
          mode="create"
          onClose={() => setCreating(false)}
          onSaved={(message) => {
            setCreating(false)
            setNotice(message)
          }}
        />
      )}
      {rotating && (
        <SecretDialog
          mode="rotate"
          secret={rotating}
          onClose={() => setRotating(null)}
          onSaved={(message) => {
            setRotating(null)
            setNotice(message)
          }}
        />
      )}
      <ConfirmDialog
        open={deleting !== null}
        title="删除密钥"
        danger
        confirmText="确认删除"
        onConfirm={() => void remove()}
        onCancel={() => setDeleting(null)}
      >
        <p className="mb-space-md">删除后密文立即丢弃且不可恢复；引用该密钥的外部系统将无法再取得它。</p>
        {deleting && <p className="font-mono text-body-sm text-on-surface">「{deleting.name}」</p>}
        {actionError && (
          <p role="alert" className="mt-space-md text-body-sm text-error">
            {actionError}
          </p>
        )}
      </ConfirmDialog>
    </>
  )
}
