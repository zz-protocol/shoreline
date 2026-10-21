import React from 'react'
import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest'
import { act, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { App as AntApp } from 'antd'
import { copyText } from './clipboard'
import { applyPoll, statsLabel, TYPE_TAG } from './eventsWindow'
import Events from './pages/Events'

beforeAll(() => {
  window.matchMedia = window.matchMedia || ((q) => ({ matches: false, media: q, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent() {} }))
  const gcs = window.getComputedStyle
  window.getComputedStyle = (el) => gcs(el)
  global.ResizeObserver = global.ResizeObserver || class { observe() {} unobserve() {} disconnect() {} }
})
afterEach(() => {
  vi.useRealTimers()
  vi.restoreAllMocks()
})

const fakeDoc = (execResult) => {
  const doc = {
    body: { appendChild: vi.fn(), removeChild: vi.fn() },
    createElement: () => ({ style: {}, setAttribute() {}, select: vi.fn() }),
    execCommand: vi.fn(() => {
      if (execResult instanceof Error) throw execResult
      return execResult
    }),
  }
  return doc
}

describe('T25-1 复制回退', () => {
  it('Clipboard API 可用 → clipboard', async () => {
    const nav = { clipboard: { writeText: vi.fn(async () => {}) } }
    const doc = fakeDoc(true)
    expect(await copyText('a\nb', nav, doc)).toBe('clipboard')
    expect(nav.clipboard.writeText).toHaveBeenCalledWith('a\nb')
    expect(doc.execCommand).not.toHaveBeenCalled()
  })
  it('Clipboard API 拒绝（非安全上下文）→ textarea + execCommand', async () => {
    const nav = { clipboard: { writeText: vi.fn(async () => { throw new Error('NotAllowedError') }) } }
    const doc = fakeDoc(true)
    expect(await copyText('x', nav, doc)).toBe('execCommand')
    expect(doc.execCommand).toHaveBeenCalledWith('copy')
    expect(doc.body.removeChild).toHaveBeenCalled()
  })
  it('无 Clipboard API（HTTP 内网）且 execCommand 失败/抛错 → false', async () => {
    expect(await copyText('x', {}, fakeDoc(false))).toBe(false)
    expect(await copyText('x', {}, fakeDoc(new Error('boom')))).toBe(false)
  })
})

describe('T25-3 / T25-5 轮询溢出与按类型统计', () => {
  it('applyPoll：本页还有 next_cursor → overflow，不合并', () => {
    const prev = [{ id: 3 }]
    expect(applyPoll(prev, { items: [{ id: 9 }], next_cursor: '5' })).toEqual({ items: prev, overflow: true })
    expect(applyPoll(prev, { items: [{ id: 4 }], next_cursor: null })).toEqual({ items: [{ id: 4 }, { id: 3 }], overflow: false })
  })
  it('statsLabel / TYPE_TAG', () => {
    expect(statsLabel({ new: 4, recovered: 1 })).toBe('新增 4 · 恢复 1')
    expect(statsLabel(undefined)).toBe('新增 0 · 恢复 0')
    expect(TYPE_TAG.new.label).toBe('新增')
    expect(TYPE_TAG.recovered.label).toBe('恢复')
    expect(TYPE_TAG.changed.label).toBe('变更')
  })
})

// 页面：fetch 按路径返回；轮询返回带 next_cursor 的满页 → 顶部提示，点击后重拉；复制全部失败 → 弹文本框
describe('Events 页面（模拟后端）', () => {
  const ev = (id, type = 'new') => ({ id, type, level: 'asset', subdomain: `h${id}.t`, vendor_id: 1, created_at: '2026-10-21T00:00:00Z' })
  const setup = () => {
    let listCalls = 0
    global.fetch = vi.fn(async (url) => {
      const u = String(url)
      let body
      if (u.startsWith('/api/vendors')) body = { items: [{ id: 1, name: 'V1' }] }
      else if (u.startsWith('/api/events/stats')) body = { items: [{ vendor_id: 1, count: 3, new: 2, recovered: 1 }], total: 3, totals: { new: 2, recovered: 1 } }
      else if (u.startsWith('/api/events/values')) body = { field: 'subdomain', items: ['h1.t', 'h2.t'], count: 2, truncated: false }
      else if (u.includes('after_id')) body = { items: Array.from({ length: 500 }, (_, i) => ev(1000 + i)), next_cursor: '1000' }
      else {
        listCalls++
        body = { items: [ev(3, 'recovered'), ev(2), ev(1)], next_cursor: null }
      }
      return { ok: true, status: 200, text: async () => JSON.stringify(body) }
    })
    return () => listCalls
  }

  it('统计「新增 X · 恢复 Y」、类型标签分开；轮询溢出提示并可刷新', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    const calls = setup()
    render(<AntApp><Events /></AntApp>)
    await waitFor(() => expect(screen.getByText('时间窗内 新增 2 · 恢复 1')).toBeTruthy())
    expect(screen.getByText('V1：新增 2 · 恢复 1')).toBeTruthy()
    expect(screen.getAllByText('恢复').length).toBeGreaterThan(0)
    expect(screen.getAllByText('新增').length).toBe(2)
    expect(calls()).toBe(1)
    await act(async () => { await vi.advanceTimersByTimeAsync(30000) })
    await waitFor(() => expect(screen.getByText('有更多新事件，点此刷新')).toBeTruthy())
    expect(document.querySelectorAll('tbody tr.ant-table-row').length).toBe(3) // 未合并 500 条
    fireEvent.click(screen.getByText('有更多新事件，点此刷新'))
    await waitFor(() => expect(calls()).toBe(2))
    await waitFor(() => expect(screen.queryByText('有更多新事件，点此刷新')).toBeNull())
  })

  it('复制：clipboard 与 execCommand 都失败 → 弹出文本框手动复制', async () => {
    setup()
    Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText: async () => { throw new Error('denied') } } })
    document.execCommand = vi.fn(() => false)
    render(<AntApp><Events /></AntApp>)
    await waitFor(() => expect(screen.getAllByText('复制本列').length).toBe(2))
    fireEvent.click(screen.getAllByText('复制本列')[0])
    await waitFor(() => expect(screen.getByTestId('manual-copy')).toBeTruthy())
    expect(screen.getByTestId('manual-copy').value).toBe('h1.t\nh2.t')
    expect(document.execCommand).toHaveBeenCalledWith('copy')
    expect(screen.getByText(/请手动复制（2 条）/)).toBeTruthy()
  })
})
