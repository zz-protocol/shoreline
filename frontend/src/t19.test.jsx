import React from 'react'
import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest'
import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { App as AntApp } from 'antd'
import { filterParams } from './eventsWindow'
import Events from './pages/Events'

beforeAll(() => {
  window.matchMedia = window.matchMedia || ((q) => ({ matches: false, media: q, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent() {} }))
  global.ResizeObserver = global.ResizeObserver || class { observe() {} unobserve() {} disconnect() {} }
})
afterEach(() => vi.restoreAllMocks())

const typeOf = (u) => new URL(u, 'http://x').searchParams.get('type')

describe('T19 类型多选', () => {
  it('filterParams：默认/空数组 → new,recovered；显式传入照传', () => {
    expect(filterParams({ level: 'asset' }).get('type')).toBe('new,recovered')
    expect(filterParams({ level: 'asset', types: [] }).get('type')).toBe('new,recovered')
    expect(filterParams({ level: 'asset', types: ['changed'] }).get('type')).toBe('changed')
  })

  it('页面：默认新增+恢复；加选变更后列表/统计/复制都带 type；全部取消回到默认', async () => {
    const urls = []
    global.fetch = vi.fn(async (url) => {
      const u = String(url)
      urls.push(u)
      let body
      if (u.startsWith('/api/vendors')) body = { items: [] }
      else if (u.startsWith('/api/events/stats')) body = { items: [], total: 0, totals: {} }
      else if (u.startsWith('/api/events/values')) body = { field: 'subdomain', items: ['h.example.test'], count: 1 }
      else body = { items: [{ id: 1, type: 'new', level: 'asset', subdomain: 'h.example.test', vendor_id: 1, created_at: '2026-10-21T00:00:00Z' }], next_cursor: null }
      return { ok: true, status: 200, text: async () => JSON.stringify(body) }
    })
    Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText: async () => {} } })
    render(<AntApp><Events /></AntApp>)
    const last = (p) => urls.filter((u) => u.startsWith(p) && (p !== '/api/events?' || !u.includes('after_id'))).at(-1)
    await waitFor(() => expect(last('/api/events/stats')).toBeTruthy())
    expect(typeOf(last('/api/events?'))).toBe('new,recovered')
    expect(typeOf(last('/api/events/stats'))).toBe('new,recovered')
    const sel = screen.getByTestId('type-select')
    const tags = () => [...sel.querySelectorAll('.ant-select-selection-item-content')].map((e) => e.textContent)
    expect(tags()).toEqual(['新增', '恢复'])

    // 加选「变更」
    fireEvent.mouseDown(sel.querySelector('.ant-select-selector'))
    const opt = await waitFor(() => [...document.querySelectorAll('.ant-select-item-option')].find((e) => e.textContent === '变更'))
    fireEvent.click(opt)
    await waitFor(() => expect(typeOf(last('/api/events/stats'))).toBe('new,recovered,changed'))
    expect(typeOf(last('/api/events?'))).toBe('new,recovered,changed')
    fireEvent.click(screen.getAllByText('复制本列')[0])
    await waitFor(() => expect(last('/api/events/values')).toBeTruthy())
    expect(typeOf(last('/api/events/values'))).toBe('new,recovered,changed')

    // 逐个取消全部 → 回到默认
    for (let i = 0; i < 3; i++) fireEvent.click(sel.querySelector('.ant-select-selection-item-remove'))
    await waitFor(() => expect(tags()).toEqual(['新增', '恢复']))
    await waitFor(() => expect(typeOf(last('/api/events/stats'))).toBe('new,recovered'))
    expect(typeOf(last('/api/events?'))).toBe('new,recovered')
  })
})

describe('T19 打回①：统计按所选类型显示', async () => {
  const { statsLabel } = await import('./eventsWindow')
  it('statsLabel 只显示已选类型', () => {
    const s = { new: 4, recovered: 1, changed: 2 }
    expect(statsLabel(s)).toBe('新增 4 · 恢复 1')
    expect(statsLabel(s, ['new', 'recovered', 'changed'])).toBe('新增 4 · 恢复 1 · 变更 2')
    expect(statsLabel(s, ['changed'])).toBe('变更 2')
    expect(statsLabel(s, ['changed', 'new'])).toBe('新增 4 · 变更 2')
  })
  it('页面只选变更：总计与各厂商显示「变更 N」，不出现「新增 0 · 恢复 0」', async () => {
    global.fetch = vi.fn(async (url) => {
      const u = String(url)
      let body
      if (u.startsWith('/api/vendors')) body = { items: [{ id: 1, name: 'V1' }] }
      else if (u.startsWith('/api/events/stats')) {
        const t = typeOf(u)
        body = t === 'changed'
          ? { items: [{ vendor_id: 1, count: 3, new: 0, recovered: 0, changed: 3 }], total: 3, totals: { new: 0, recovered: 0, changed: 3 } }
          : { items: [{ vendor_id: 1, count: 2, new: 2, recovered: 0, changed: 0 }], total: 2, totals: { new: 2, recovered: 0, changed: 0 } }
      } else body = { items: [], next_cursor: null }
      return { ok: true, status: 200, text: async () => JSON.stringify(body) }
    })
    render(<AntApp><Events /></AntApp>)
    await waitFor(() => expect(screen.getByText('时间窗内 新增 2 · 恢复 0')).toBeTruthy())
    const sel = screen.getAllByTestId('type-select').at(-1)
    fireEvent.mouseDown(sel.querySelector('.ant-select-selector'))
    const opt = await waitFor(() => [...document.querySelectorAll('.ant-select-item-option')].find((e) => e.textContent === '变更'))
    fireEvent.click(opt)
    fireEvent.click(sel.querySelector('.ant-select-selection-item-remove')) // 去掉新增
    fireEvent.click(sel.querySelector('.ant-select-selection-item-remove')) // 去掉恢复
    await waitFor(() => expect(screen.getByText('时间窗内 变更 3')).toBeTruthy())
    expect(screen.getByText('V1：变更 3')).toBeTruthy()
    expect(screen.queryByText(/新增 0 · 恢复 0/)).toBeNull()
  })
})
