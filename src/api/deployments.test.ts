import { describe, expect, it } from 'vitest'
import { lastCheckText } from './deployments'

describe('lastCheckText', () => {
  it('reports a target that was never probed', () => {
    expect(lastCheckText({ lastCheckAt: 0, lastCheckOk: false, lastCheckDetail: '' })).toBe('未检查')
  })

  it('summarizes a healthy probe', () => {
    const now = Math.floor(Date.now() / 1000)
    expect(lastCheckText({ lastCheckAt: now, lastCheckOk: true, lastCheckDetail: 'ok' })).toBe(
      '健康 · HTTP 2xx · 刚刚',
    )
  })

  it('keeps the failure detail and marks it abnormal', () => {
    const now = Math.floor(Date.now() / 1000)
    expect(lastCheckText({ lastCheckAt: now, lastCheckOk: false, lastCheckDetail: 'http 500' })).toBe(
      '异常 · http 500 · 刚刚',
    )
  })
})
