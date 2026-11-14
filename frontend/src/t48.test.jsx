import React from 'react'
import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest'
import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { App as AntApp } from 'antd'
import Components from './pages/Components'

beforeAll(() => {
  window.matchMedia = window.matchMedia || ((q) => ({ matches: false, media: q, addListener() {}, removeListener() {}, addEventListener() {}, removeListener() {}, dispatchEvent() {} }))
  global.ResizeObserver = global.ResizeObserver || class { observe() {} unobserve() {} disconnect() {} }
})
afterEach(() => vi.restoreAllMocks())

const pageRows = [
  { id: 1, name: 'Django', source: 'wappalyzer', categories: ['Web frameworks'], url_count: 1, asset_count: 1, vendor_count: 1, is_common: false },
]
const allNames = ['CrossSSR', 'Django', 'EmptyCat']

describe('T48 组件列表整列复制', () => {
  it('复制本列请求 /api/components/values，内容为当前筛选全量而非本页', async () => {
    const copied = []
    Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText: async (t) => { copied.push(t) } } })
    const valuesURLs = []
    global.fetch = vi.fn(async (url) => {
      const u = String(url)
      let body = { items: [] }
      if (u.startsWith('/api/component-candidates')) body = { configured: false, items: [] }
      else if (u.startsWith('/api/vendors')) body = { items: [] }
      else if (u.startsWith('/api/components/values')) {
        valuesURLs.push(u)
        body = { field: 'name', items: allNames, count: allNames.length, truncated: false }
      } else if (u.startsWith('/api/components?')) {
        body = { items: pageRows, next_cursor: '99' }
      }
      return { ok: true, status: 200, text: async () => JSON.stringify(body) }
    })
    render(<AntApp><Components /></AntApp>)
    await waitFor(() => expect(screen.getByText('Django')).toBeTruthy())
    expect(screen.getByTestId('copy-comp-name')).toBeTruthy()
    fireEvent.click(screen.getByTestId('copy-comp-name'))
    await waitFor(() => expect(copied).toEqual([allNames.join('\n')]))
    expect(valuesURLs[0]).toMatch(/^\/api\/components\/values\?/)
    expect(valuesURLs[0]).toContain('field=name')
    expect(valuesURLs[0]).toContain('include_all_categories=false')
    expect(screen.queryByText('CrossSSR')).toBeNull()
  })

  it('HTTP 回退失败时弹出手动复制框，truncated 时标题注明截断', async () => {
    Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText: async () => { throw new Error('denied') } } })
    document.execCommand = vi.fn(() => false)
    global.fetch = vi.fn(async (url) => {
      const u = String(url)
      let body = { items: [] }
      if (u.startsWith('/api/component-candidates')) body = { configured: false, items: [] }
      else if (u.startsWith('/api/vendors')) body = { items: [] }
      else if (u.startsWith('/api/components/values')) body = { field: 'name', items: ['A', 'B'], count: 2, truncated: true }
      else if (u.startsWith('/api/components?')) body = { items: pageRows, next_cursor: null }
      return { ok: true, status: 200, text: async () => JSON.stringify(body) }
    })
    render(<AntApp><Components /></AntApp>)
    await waitFor(() => expect(screen.getByTestId('copy-comp-name')).toBeTruthy())
    fireEvent.click(screen.getByTestId('copy-comp-name'))
    await waitFor(() => expect(screen.getByTestId('manual-copy')).toBeTruthy())
    expect(screen.getByTestId('manual-copy').value).toBe('A\nB')
    expect(screen.getByText(/已截断至上限/)).toBeTruthy()
    expect(document.execCommand).toHaveBeenCalledWith('copy')
  })
})
