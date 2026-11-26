import { useId, useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { Button, Field, Input } from '@/components/ui/DataTable'
import { useDialogFocus } from '@/components/ui/useDialogFocus'
import { secretsAPI, type SecretMetadata } from '@/api/secrets'
import { secretError, secretCreateValidation, secretRotateValidation } from './secretForm'

/**
 * 密钥新建/轮换对话框。值只写不读：输入框始终是密码框，表单要求二次确认，保存后
 * 页面不会再显示该值。复用现有模态语义（role=dialog + aria-modal、焦点圈定、Esc）。
 */
export function SecretDialog({
  mode,
  secret,
  onClose,
  onSaved,
}: {
  mode: 'create' | 'rotate'
  /** Present for rotate. */
  secret?: SecretMetadata
  onClose: () => void
  onSaved: (message: string) => void
}) {
  const queryClient = useQueryClient()
  const dialogRef = useDialogFocus(true, onClose)
  const titleId = useId()
  const [name, setName] = useState('')
  const [value, setValue] = useState('')
  const [confirm, setConfirm] = useState('')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)

  const submit = async (event: React.FormEvent) => {
    event.preventDefault()
    if (busy) return
    setError('')
    const invalid =
      mode === 'create'
        ? secretCreateValidation({ name, value, confirm })
        : secretRotateValidation({ value, confirm })
    if (invalid) {
      setError(invalid)
      return
    }
    setBusy(true)
    try {
      if (mode === 'create') {
        const saved = await secretsAPI.create({ name: name.trim(), value })
        await queryClient.invalidateQueries({ queryKey: ['secrets'] })
        onSaved(`已保存密钥「${saved.name}」；值不会再次显示。`)
      } else if (secret) {
        await secretsAPI.rotate(secret.id, { value })
        await queryClient.invalidateQueries({ queryKey: ['secrets'] })
        onSaved(`已轮换密钥「${secret.name}」；旧值已失效。`)
      }
    } catch (cause) {
      setError(secretError(cause))
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="fixed inset-0 z-modal bg-black/60 flex items-center justify-center" onClick={onClose}>
      <div
        ref={dialogRef}
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleId}
        tabIndex={-1}
        className="bg-surface-container rounded-lg border border-surface-container-highest w-[520px] max-h-[88vh] overflow-y-auto p-space-xl focus:outline-none"
        onClick={(event) => event.stopPropagation()}
      >
        <form onSubmit={submit} aria-busy={busy} className="space-y-space-lg">
          <h3 id={titleId} className="text-headline-md font-semibold text-on-surface">
            {mode === 'create' ? '新建密钥' : `轮换密钥「${secret?.name ?? ''}」`}
          </h3>
          <p className="text-body-sm text-on-surface-variant">
            值在服务端以 AES-256-GCM 加密存储，保存后<strong className="font-semibold text-on-surface">无法再次查看</strong>
            ，请确认已安全保存。填写后不会回传、不会出现在日志或审计中。
          </p>
          {mode === 'create' ? (
            <Field label="名称" hint="必填；单行，最多 128 字节，名称唯一" htmlFor="secret-name">
              <Input
                id="secret-name"
                value={name}
                maxLength={128}
                autoComplete="off"
                onChange={(event) => setName(event.target.value)}
                disabled={busy}
                placeholder="例如：prod-db-password"
              />
            </Field>
          ) : (
            <p className="text-body-sm text-on-surface">
              名称：<span className="font-mono">{secret?.name}</span>
            </p>
          )}
          <Field label={mode === 'create' ? '值' : '新值'} hint="必填；1-4096 字节" htmlFor="secret-value">
            <Input
              id="secret-value"
              type="password"
              value={value}
              autoComplete="new-password"
              onChange={(event) => setValue(event.target.value)}
              disabled={busy}
            />
          </Field>
          <Field label="确认值" hint="再次输入以确认；两次必须一致" htmlFor="secret-confirm">
            <Input
              id="secret-confirm"
              type="password"
              value={confirm}
              autoComplete="new-password"
              onChange={(event) => setConfirm(event.target.value)}
              disabled={busy}
            />
          </Field>
          {error && (
            <p role="alert" className="text-body-sm text-error bg-error/10 rounded px-space-lg py-space-md">
              {error}
            </p>
          )}
          <div className="flex justify-end gap-space-md pt-space-sm">
            <Button variant="secondary" disabled={busy} onClick={onClose}>
              取消
            </Button>
            <Button type="submit" disabled={busy}>
              {busy ? '保存中...' : mode === 'create' ? '保存密钥' : '确认轮换'}
            </Button>
          </div>
        </form>
      </div>
    </div>
  )
}
