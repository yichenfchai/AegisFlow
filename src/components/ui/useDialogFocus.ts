import { useEffect, useRef } from 'react'

/** 与浏览器 Tab 序一致的基础可聚焦元素集合。 */
const FOCUSABLE = [
  'a[href]',
  'button:not([disabled])',
  'input:not([disabled])',
  'select:not([disabled])',
  'textarea:not([disabled])',
  '[tabindex]:not([tabindex="-1"])',
].join(',')

function focusableInside(node: HTMLElement): HTMLElement[] {
  return Array.from(node.querySelectorAll<HTMLElement>(FOCUSABLE)).filter(
    (element) => !element.hasAttribute('hidden') && element.getAttribute('aria-hidden') !== 'true',
  )
}

/**
 * 模态对话框的语义与焦点管理：打开时把焦点交给对话框内首个可操作元素，
 * Tab/Shift+Tab 在对话框内循环，Esc 触发 onClose，关闭后把焦点归还给打开
 * 对话框前的触发元素（触发元素已从文档移除时交给浏览器默认处理）。
 *
 * 调用方需把返回的 ref 挂在 role="dialog" 的容器上，并自行提供 aria-modal
 * 与 aria-labelledby；本 hook 不负责渲染结构。
 */
export function useDialogFocus(open: boolean, onClose: () => void) {
  const dialogRef = useRef<HTMLDivElement>(null)
  const onCloseRef = useRef(onClose)
  const triggerRef = useRef<HTMLElement | null>(null)

  useEffect(() => {
    onCloseRef.current = onClose
  })

  useEffect(() => {
    if (!open) return
    const dialog = dialogRef.current
    if (!dialog) return
    const active = document.activeElement
    triggerRef.current = active instanceof HTMLElement && active !== document.body ? active : null
    const initial = focusableInside(dialog)[0] ?? dialog
    initial.focus()

    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === 'Escape') {
        event.preventDefault()
        onCloseRef.current()
        return
      }
      if (event.key !== 'Tab') return
      const items = focusableInside(dialog)
      if (items.length === 0) {
        event.preventDefault()
        dialog.focus()
        return
      }
      const first = items[0]
      const last = items[items.length - 1]
      const backwards = event.shiftKey
      if (!dialog.contains(document.activeElement)) {
        event.preventDefault()
        ;(backwards ? last : first).focus()
        return
      }
      if (!backwards && document.activeElement === last) {
        event.preventDefault()
        first.focus()
      } else if (backwards && document.activeElement === first) {
        event.preventDefault()
        last.focus()
      }
    }

    document.addEventListener('keydown', onKeyDown)
    const trigger = triggerRef.current
    return () => {
      document.removeEventListener('keydown', onKeyDown)
      if (trigger?.isConnected) trigger.focus()
    }
  }, [open])

  return dialogRef
}
