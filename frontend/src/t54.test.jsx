import React from 'react'
import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest'
import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { App as AntApp } from 'antd'
import Components from './pages/Components'

beforeAll(() => {
  window.matchMedia = window.matchMedia || ((q) => ({ matches: false, media: q, addListener() {}, removeListener() {}, addEventListener() {}, dispatchEvent() {} }))
  global.ResizeObserver = global.ResizeObserver || class { observe() {} unobserve() {} disconnect() {} }
})
afterEach(() => vi.restoreAllMocks())

const rows = [
  {
    id: 1, name: 'RareQueue', source: 'service', categories: ['Message queues'],
    url_count: 0, asset_count: 3, vendor_count: 1, is_common: false,
    research_score: 87.5, research_score_parts: { attack: 1, source: 1, advisory: 1, usage: 0.5 },
  },
  {
    id: 2, name: 'PlainWeb', source: 'wappalyzer', categories: ['Web servers'],
    url_count: 1, asset_count: 1, vendor_count: 1, is_common: false,
    research_score: 35, research_score_parts: { attack: 0.4, source: 0.5, advisory: 0.5, usage: 0 },
  },
]

function mockFetch(requests) {
  global.fetch = vi.fn(async (url) => {
    const u = String(url)
    requests.push(u)
    let body = { items: [] }
    if (u.startsWith('/api/component-candidates')) body = { configured: false, items: [] }
    else if (u.startsWith('/api/vendors')) body = { items: [] }
    else if (u.startsWith('/api/components/values')) body = { field: 'name', items: ['PlainWeb', 'RareQueue'], count: 2, truncated: false }
    else if (u.startsWith('/api/components?')) body = { items: rows, next_cursor: null }
    return { ok: true, status: 200, text: async () => JSON.stringify(body) }
  })
}

describe('T54 研究价值评分', () => {
  it('展示 0–100 分（1 位小数），tooltip 给出四分项', async () => {
    const requests = []
    mockFetch(requests)
    render(<AntApp><Components /></AntApp>)
    await waitFor(() => expect(screen.getByText('RareQueue')).toBeTruthy())
    expect(screen.getByText('研究价值')).toBeTruthy()
    expect(screen.getByTestId('research-score-1').textContent).toBe('87.5')
    expect(screen.getByTestId('research-score-2').textContent).toBe('35.0')

    fireEvent.mouseEnter(screen.getByTestId('research-score-1'))
    await waitFor(() => {
      const tip = screen.getByText(/攻击面：1\.00/)
      expect(tip.textContent).toContain('源码可得性：1.00')
      expect(tip.textContent).toContain('披露稀缺：1.00')
      expect(tip.textContent).toContain('本 SRC 频度：0.50')
    })
  })

  it('排序与最低分进入列表请求，复制本列沿用同一筛选', async () => {
    Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText: async () => {} } })
    const requests = []
    mockFetch(requests)
    render(<AntApp><Components /></AntApp>)
    await waitFor(() => expect(screen.getByText('RareQueue')).toBeTruthy())

    fireEvent.change(screen.getByTestId('comp-sort'), { target: { value: 'research' } })
    await waitFor(() => expect(requests.some((u) => u.startsWith('/api/components?') && u.includes('sort=research'))).toBe(true))

    fireEvent.change(screen.getByTestId('min-score'), { target: { value: '60' } })
    await waitFor(() => expect(requests.some((u) => u.startsWith('/api/components?') && u.includes('min_score=60'))).toBe(true))

    fireEvent.click(screen.getByTestId('copy-comp-name'))
    await waitFor(() => expect(requests.some((u) => u.startsWith('/api/components/values') && u.includes('min_score=60'))).toBe(true))
    const copyURL = requests.find((u) => u.startsWith('/api/components/values'))
    expect(copyURL).toContain('sort=research')
    expect(copyURL).toContain('field=name')
  })

  it('组件列表与请求都不含 IP', async () => {
    const requests = []
    mockFetch(requests)
    render(<AntApp><Components /></AntApp>)
    await waitFor(() => expect(screen.getByText('RareQueue')).toBeTruthy())
    expect(screen.queryByText('IP')).toBeNull()
    expect(document.body.textContent).not.toMatch(/\b\d{1,3}(\.\d{1,3}){3}\b/)
    for (const u of requests) {
      expect(u.toLowerCase()).not.toMatch(/[?&]ip=/)
    }
  })
})
