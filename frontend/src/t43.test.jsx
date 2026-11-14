import React from 'react'
import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest'
import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { App as AntApp } from 'antd'
import { MemoryRouter, Route, Routes } from 'react-router-dom'
import AppLayout from './layout/AppLayout'
import Components from './pages/Components'

beforeAll(() => {
  window.matchMedia = window.matchMedia || ((q) => ({ matches: false, media: q, addListener() {}, removeListener() {}, addEventListener() {}, removeListener() {}, dispatchEvent() {} }))
  global.ResizeObserver = global.ResizeObserver || class { observe() {} unobserve() {} disconnect() {} }
})
afterEach(() => vi.restoreAllMocks())

describe('T43 组件页', () => {
  it('导航「组件」在「资产」之后，页面全宽', () => {
    render(
      <MemoryRouter initialEntries={['/components']}>
        <Routes>
          <Route path="/" element={<AppLayout />}>
            <Route path="components" element={<div>组件正文</div>} />
          </Route>
        </Routes>
      </MemoryRouter>,
    )
    const labels = [...document.querySelectorAll('.ant-menu-item, .ant-menu-overflow-item')].map((e) => e.textContent)
    const unique = labels.filter((t, i) => t && labels.indexOf(t) === i)
    const ai = unique.indexOf('资产')
    const ci = unique.indexOf('组件')
    expect(ai).toBeGreaterThanOrEqual(0)
    expect(ci).toBe(ai + 1)
    expect(document.querySelector('.page-content').getAttribute('data-wide')).toBe('1')
  })

  it('默认排除常见；列表列齐全；下钻并按当前筛选复制', async () => {
    const urls = []
    const copied = []
    Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText: async (t) => { copied.push(t) } } })
    global.fetch = vi.fn(async (url) => {
      const u = String(url)
      urls.push(u)
      let body = { items: [] }
      if (u.startsWith('/api/vendors')) body = { items: [{ id: 1, name: 'V1' }, { id: 2, name: 'V2' }] }
      else if (u.startsWith('/api/components?') && !u.includes('/assets') && !u.includes('/values')) {
        body = { items: [
          { id: 10, name: 'RareCMS', source: 'wappalyzer', url_count: 2, asset_count: 2, vendor_count: 2, is_common: false },
        ], next_cursor: null }
      } else if (u.startsWith('/api/components/10/assets')) {
        body = { items: [{ id: 1, subdomain: 'a.v1.example.test', vendor: 'V1', vendor_id: 1, urls: [{ id: 11, url: 'https://a.v1.example.test:443', title: '', version: '' }] }] }
      } else if (u.startsWith('/api/components/10/values')) {
        const field = new URL(u, 'http://x').searchParams.get('field')
        body = field === 'url'
          ? { field: 'url', items: ['https://a.v1.example.test:443'], count: 1, truncated: false }
          : { field: 'subdomain', items: ['a.v1.example.test'], count: 1, truncated: false }
      }
      return { ok: true, status: 200, text: async () => JSON.stringify(body) }
    })
    render(<AntApp><Components /></AntApp>)
    await waitFor(() => expect(screen.getByText('RareCMS')).toBeTruthy())
    expect(screen.getByText('组件名')).toBeTruthy()
    expect(screen.getByText('来源')).toBeTruthy()
    expect(screen.getByText('URL 数')).toBeTruthy()
    expect(screen.getByText('资产数')).toBeTruthy()
    expect(screen.getByText('厂商数')).toBeTruthy()
    expect(screen.getByText('是否常见')).toBeTruthy()
    const listCall = urls.find((u) => u.startsWith('/api/components?'))
    expect(listCall).toContain('exclude_common=true')
    expect(listCall).toContain('include_wildcard=false')
    expect(listCall).toContain('include_customer_hosted=false')
    expect(listCall).toContain('include_dead=false')

    fireEvent.click(screen.getByText('RareCMS'))
    await waitFor(() => expect(screen.getByTestId('copy-comp-subdomain')).toBeTruthy())
    fireEvent.click(screen.getByTestId('copy-comp-subdomain'))
    await waitFor(() => expect(copied).toEqual(['a.v1.example.test']))
    const val = urls.find((u) => u.includes('/values?'))
    expect(val).toContain('field=subdomain')
    expect(val).toContain('include_wildcard=false')
  })
})
