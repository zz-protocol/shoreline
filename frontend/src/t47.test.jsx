import React from 'react'
import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest'
import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { App as AntApp } from 'antd'
import Components, { HIDDEN_CATEGORIES, SHOWN_CATEGORY_EXAMPLES } from './pages/Components'

beforeAll(() => {
  window.matchMedia = window.matchMedia || ((q) => ({ matches: false, media: q, addListener() {}, removeListener() {}, addEventListener() {}, removeListener() {}, dispatchEvent() {} }))
  global.ResizeObserver = global.ResizeObserver || class { observe() {} unobserve() {} disconnect() {} }
})
afterEach(() => vi.restoreAllMocks())

const rows = {
  default: [
    { id: 1, name: 'Django', source: 'wappalyzer', categories: ['Web frameworks'], url_count: 2, asset_count: 2, vendor_count: 2, is_common: false },
    { id: 2, name: 'Next.js', source: 'wappalyzer', categories: ['JavaScript frameworks', 'Web frameworks'], url_count: 1, asset_count: 1, vendor_count: 1, is_common: false },
  ],
  all: [
    { id: 1, name: 'Django', source: 'wappalyzer', categories: ['Web frameworks'], url_count: 2, asset_count: 2, vendor_count: 2, is_common: false },
    { id: 2, name: 'Next.js', source: 'wappalyzer', categories: ['JavaScript frameworks', 'Web frameworks'], url_count: 1, asset_count: 1, vendor_count: 1, is_common: false },
    { id: 3, name: 'Lodash', source: 'wappalyzer', categories: ['JavaScript libraries'], url_count: 1, asset_count: 1, vendor_count: 1, is_common: false },
  ],
}

describe('T47 组件分类降噪', () => {
  it('锁定隐藏集与显示示例，行显示分类标签；默认不请求展开，打开后可见纯前端', async () => {
    expect(HIDDEN_CATEGORIES).toContain('JavaScript libraries')
    expect(HIDDEN_CATEGORIES).toContain('CDN')
    expect(SHOWN_CATEGORY_EXAMPLES).toContain('Web frameworks')
    const calls = []
    global.fetch = vi.fn(async (url) => {
      const u = String(url)
      calls.push(u)
      let body = { items: [] }
      if (u.startsWith('/api/component-candidates')) body = { configured: false, items: [] }
      else if (u.startsWith('/api/vendors')) body = { items: [] }
      else if (u.startsWith('/api/components?')) {
        const q = new URL(u, 'http://ui.example.test')
        body = { items: q.searchParams.get('include_all_categories') === 'true' ? rows.all : rows.default, next_cursor: null }
      }
      return { ok: true, status: 200, text: async () => JSON.stringify(body) }
    })
    render(<AntApp><Components /></AntApp>)
    await waitFor(() => expect(screen.getByText('Django')).toBeTruthy())
    expect(screen.getByTestId('comp-cats-1').textContent).toContain('Web frameworks')
    expect(screen.getByText('Next.js')).toBeTruthy()
    expect(screen.getByTestId('comp-cats-2').textContent).toContain('JavaScript frameworks')
    expect(screen.queryByText('Lodash')).toBeNull()
    expect(screen.getByTestId('include-all-categories')).toBeTruthy()
    expect(screen.getByTestId('category-JavaScript libraries')).toBeTruthy()
    expect(screen.getByTestId('category-Web frameworks')).toBeTruthy()
    const firstList = calls.filter((u) => u.startsWith('/api/components?'))[0]
    expect(firstList).toContain('include_all_categories=false')
    fireEvent.click(screen.getByTestId('include-all-categories'))
    await waitFor(() => expect(screen.getByText('Lodash')).toBeTruthy())
    expect(screen.getByTestId('comp-cats-3').textContent).toContain('JavaScript libraries')
  })
})
