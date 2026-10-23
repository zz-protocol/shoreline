import React from 'react'
import { describe, expect, it, vi, beforeAll, afterEach } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import { App as AntApp } from 'antd'
import { assetFilterParams } from './pages/Assets'
import Assets from './pages/Assets'

beforeAll(() => {
  window.matchMedia = window.matchMedia || ((q) => ({ matches: false, media: q, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent() {} }))
  global.ResizeObserver = global.ResizeObserver || class { observe() {} unobserve() {} disconnect() {} }
})
afterEach(() => vi.restoreAllMocks())

describe('T29 assetFilterParams', () => {
  it('列表与复制共用筛选；空 q / 不限存活时间不传对应参数', () => {
    const p = assetFilterParams({ vendor: 3, q: '  Api. ', wild: true, hosted: false, alive: 'alive', aliveWin: '' })
    expect(p.get('vendor')).toBe('3')
    expect(p.get('q')).toBe('Api.')
    expect(p.get('include_wildcard')).toBe('true')
    expect(p.get('include_customer_hosted')).toBe('false')
    expect(p.get('alive')).toBe('alive')
    expect(p.has('alive_since')).toBe(false)
    expect(assetFilterParams({ aliveWin: '' }).has('q')).toBe(false)
  })
  it('近 7 天带 alive_since', () => {
    const p = assetFilterParams({ aliveWin: '7d' })
    expect(p.has('alive_since')).toBe(true)
  })
})

describe('T29 补丁：URL 列单元格展示文本', () => {
  it('列表行显示 url 文本；列头仍有复制本列', async () => {
    global.fetch = vi.fn(async (url) => {
      const u = String(url)
      let body
      if (u.startsWith('/api/vendors')) body = { items: [] }
      else if (u.startsWith('/api/assets?')) {
        body = {
          items: [{
            id: 1, subdomain: 'a.example.test', url: 'https://a.example.test:443\nhttps://a.example.test:8443',
            alive: 'alive', in_scope: true, wildcard: false, customer_hosted: false, sources: ['crtsh'],
          }],
          next_cursor: null,
        }
      } else body = { items: [] }
      return { ok: true, status: 200, text: async () => JSON.stringify(body) }
    })
    render(<AntApp><Assets /></AntApp>)
    await waitFor(() => expect(screen.getByTestId('asset-url').textContent).toContain('https://a.example.test:443'))
    expect(screen.getByTestId('asset-url').textContent).toContain('https://a.example.test:8443')
    expect(screen.getByTestId('copy-url')).toBeTruthy()
    expect(screen.getByTestId('copy-subdomain')).toBeTruthy()
  })
})
