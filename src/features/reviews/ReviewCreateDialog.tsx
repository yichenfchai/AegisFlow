import { useId, useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { Button, Field, Input, Select, Textarea } from '@/components/ui/DataTable'
import { useDialogFocus } from '@/components/ui/useDialogFocus'
import { reviewsAPI, type Review } from '@/api/reviews'
import type { Project } from '@/api/projects'
import { reviewCreateError, reviewCreateValidation } from './reviewForm'

/**
 * 发起审查对话框。复用现有 ConfirmDialog 的模态语义：role=dialog + aria-modal、
 * aria-labelledby、Tab 焦点圈定、Esc 关闭与关闭后焦点归还（useDialogFocus）。
 * 项目下拉来自调用方已有的可见项目列表，避免重复请求。
 */
export function ReviewCreateDialog({
  projects,
  onClose,
  onCreated,
}: {
  projects: Project[]
  onClose: () => void
  onCreated: (review: Review) => void
}) {
  const queryClient = useQueryClient()
  const dialogRef = useDialogFocus(true, onClose)
  const titleId = useId()
  const [projectId, setProjectId] = useState('')
  const [subject, setSubject] = useState('')
  const [description, setDescription] = useState('')
  const [version, setVersion] = useState('')
  const [externalUrl, setExternalUrl] = useState('')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)

  const submit = async (event: React.FormEvent) => {
    event.preventDefault()
    if (busy) return
    setError('')
    if (!projectId) {
      setError('请选择项目')
      return
    }
    const invalid = reviewCreateValidation({ subject, description, version, externalUrl })
    if (invalid) {
      setError(invalid)
      return
    }
    setBusy(true)
    try {
      const review = await reviewsAPI.create(projectId, {
        subject: subject.trim(),
        description: description.trim() || undefined,
        version: version.trim() || undefined,
        externalUrl: externalUrl.trim() || undefined,
      })
      await queryClient.invalidateQueries({ queryKey: ['reviews'] })
      onCreated(review)
    } catch (cause) {
      setError(reviewCreateError(cause))
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
        className="bg-surface-container rounded-lg border border-surface-container-highest w-[560px] max-h-[85vh] overflow-y-auto p-space-xl focus:outline-none"
        onClick={(event) => event.stopPropagation()}
      >
        <form onSubmit={submit} aria-busy={busy} className="space-y-space-lg">
          <h3 id={titleId} className="text-headline-md font-semibold text-on-surface">
            发起审查
          </h3>
          <p className="text-body-sm text-on-surface-variant">
            审查会挂到所选项目下，由该项目维护者或管理员批准/拒绝；外部链接仅作为展示引用，服务端不会抓取。
          </p>
          <Field
            label="项目"
            hint={projects.length === 0 ? '还没有可见项目；请先创建项目' : '只能在你可写的项目上发起审查'}
            htmlFor="review-create-project"
          >
            <Select
              id="review-create-project"
              value={projectId}
              onChange={(event) => setProjectId(event.target.value)}
              disabled={busy || projects.length === 0}
            >
              <option value="">请选择项目</option>
              {projects.map((project) => (
                <option key={project.id} value={project.id}>
                  {project.name}
                </option>
              ))}
            </Select>
          </Field>
          <Field label="审查主题" hint="必填；单行，3-200 字节" htmlFor="review-create-subject">
            <Input
              id="review-create-subject"
              value={subject}
              onChange={(event) => setSubject(event.target.value)}
              disabled={busy}
              placeholder="发布 v1.2.3 到生产"
            />
          </Field>
          <Field label="描述" hint="选填；最多 2000 字节" htmlFor="review-create-description">
            <Textarea
              id="review-create-description"
              value={description}
              rows={3}
              onChange={(event) => setDescription(event.target.value)}
              disabled={busy}
              placeholder="变更范围、验证方式或需要 reviewer 关注的点"
            />
          </Field>
          <div className="grid grid-cols-2 gap-space-xl">
            <Field label="版本" hint="选填；分支 / 标签 / SHA，最多 64 字节" htmlFor="review-create-version">
              <Input
                id="review-create-version"
                mono
                value={version}
                onChange={(event) => setVersion(event.target.value)}
                disabled={busy}
                placeholder="v1.2.3"
              />
            </Field>
            <Field label="外部链接" hint="选填；http(s) 绝对地址" htmlFor="review-create-external-url">
              <Input
                id="review-create-external-url"
                mono
                value={externalUrl}
                onChange={(event) => setExternalUrl(event.target.value)}
                disabled={busy}
                placeholder="https://example.com/reviews/1"
              />
            </Field>
          </div>
          {error && (
            <p role="alert" className="text-body-sm text-error bg-error/10 rounded px-space-lg py-space-md">
              {error}
            </p>
          )}
          <div className="flex justify-end gap-space-md pt-space-sm">
            <Button variant="secondary" disabled={busy} onClick={onClose}>
              取消
            </Button>
            <Button type="submit" disabled={busy || !projectId || subject.trim() === ''}>
              {busy ? '发起中...' : '发起审查'}
            </Button>
          </div>
        </form>
      </div>
    </div>
  )
}
