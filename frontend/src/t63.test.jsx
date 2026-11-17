import React from 'react'
import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest'
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { App as AntApp } from 'antd'
import Components, { EXPOSURE_STATES, EXPOSURE_LABELS } from './pages/Components'

beforeAll(() => {
  window.matchMedia = window.matchMedia || ((q) => ({ matches: false, media: q, addListener() {}, removeListener() {}, addEventListener() {}, removeListener() {}, dispatchEvent() {} }))
  global.ResizeObserver = global.ResizeObserver || class { observe() {} unobserve() {} disconnect() {} }
})
afterEach(() => vi.restoreAllMocks())

const component = {
  id: 10, name: 'RareCMS', source: 'wappalyzer', categories: ['CMS'],
  url_count: 1, asset_count: 1, vendor_count: 1, is_common: false,
  github_repo: 'owner/confirmed', stars: 12, advisory_count: 1,
  research_score: 61.5, research_score_parts: { attack: 0.5, source: 0.5, advisory: 0.5, usage: 0.5 },
}

const relation = {
  id: 7, asset_id: 7, subdomain: 'a.v1.example.test', vendor: 'V1', vendor_id: 1,
  url_id: 11, port: null, url: 'https://a.v1.example.test:443', title: 'CMS 后台',
  urls: [{ id: 11, url: 'https://a.v1.example.test:443', title: 'CMS 后台', version: '1.2' }],
  component_evidence: { type: 'header', value: 'X-Powered-By: PHP', snippet: 'x-powered-by: php/8.1' },
  component_confidence: 'high',
  matched_url: 'https://a.v1.example.test:443',
  version_hint: '1.2',
  exposure_state: 'api',
  auth_state: 'anonymous',
  component_candidate_repo: 'php/php-src',
  capture_time: '2026-11-16T10:00:00Z',
}

function mockFetch(calls) {
  global.fetch = vi.fn(async (url) => {
    const u = String(url)
    calls.push(u)
    let body = { items: [] }
    if (u.startsWith('/api/vendors')) body = { items: [] }
    else if (u.startsWith('/api/component-candidates')) body = { configured: true, items: [] }
    else if (u.startsWith('/api/components/10/assets')) body = { items: [relation], next_cursor: null }
    else if (u.startsWith('/api/components?') && !u.includes('/assets') && !u.includes('/values')) {
      body = { items: [component], next_cursor: null }
    }
    return { ok: true, status: 200, text: async () => JSON.stringify(body) }
  })
}

describe('T63 组件页 8 字段与筛选', () => {
  it('暴露状态枚举与中文标签齐全', () => {
    expect(EXPOSURE_STATES).toEqual([
      'live_app', 'login_only', 'api', 'admin', 'placeholder', 'waf', 'error_page', 'static_only', 'unknown',
    ])
    for (const v of EXPOSURE_STATES) expect(EXPOSURE_LABELS[v]).toBeTruthy()
  })

  it('min_confidence 与 exposure 进入列表筛选参数', async () => {
    const calls = []
    mockFetch(calls)
    render(<AntApp><Components /></AntApp>)
    await waitFor(() => expect(screen.getByText('RareCMS')).toBeTruthy())

    const first = calls.find((u) => u.startsWith('/api/components?'))
    expect(first).not.toContain('min_confidence')
    expect(first).not.toContain('exposure')

    fireEvent.change(screen.getByTestId('min-confidence'), { target: { value: 'high' } })
    fireEvent.click(screen.getByTestId('exposure-live_app'))
    fireEvent.click(screen.getByTestId('exposure-api'))
    await waitFor(() => {
      const last = calls.filter((u) => u.startsWith('/api/components?')).pop()
      expect(last).toContain('min_confidence=high')
      expect(last).toContain('exposure=live_app')
      expect(last).toContain('exposure=api')
    })
  })

  it('展开行展示可信度/暴露/认证/候选仓库/版本线索/采集时间', async () => {
    const calls = []
    mockFetch(calls)
    render(<AntApp><Components /></AntApp>)
    await waitFor(() => expect(screen.getByText('RareCMS')).toBeTruthy())

    const expander = document.querySelector('.ant-table-row-expand-icon')
    expect(expander).toBeTruthy()
    fireEvent.click(expander)
    await waitFor(() => expect(screen.getByText('php/php-src')).toBeTruthy())
    expect(screen.getByText('a.v1.example.test')).toBeTruthy()
    expect(calls.some((u) => u.startsWith('/api/components/10/assets'))).toBe(true)
    const html = document.body.innerHTML
    // 暴露/认证/可信度/版本线索都以中文标签或原值出现
    expect(html).toContain('API')
    expect(html).toContain('匿名')
    expect(html).toContain('高')
    expect(html).toContain('1.2')
    expect(document.querySelectorAll('table').length).toBeGreaterThan(1)
  })

  it('下钻展示 8 字段，证据可折叠，且页面无 IP', async () => {
    const calls = []
    mockFetch(calls)
    render(<AntApp><Components /></AntApp>)
    await waitFor(() => expect(screen.getByText('RareCMS')).toBeTruthy())

    fireEvent.click(screen.getByText('RareCMS'))
    await waitFor(() => expect(screen.getByTestId('comp-asset-table')).toBeTruthy())
    const table = within(screen.getByTestId('comp-asset-table'))
    expect(table.getAllByText('https://a.v1.example.test:443').length).toBeGreaterThan(0)
    expect(table.getByText('php/php-src')).toBeTruthy()
    expect(table.getByText('高')).toBeTruthy()
    expect(table.getByText('API')).toBeTruthy()
    expect(table.getByText('匿名')).toBeTruthy()
    expect(table.getByText('1.2')).toBeTruthy()

    // 证据默认只给 type/value，点开看 snippet
    expect(screen.queryByText('x-powered-by: php/8.1')).toBeNull()
    fireEvent.click(screen.getAllByTestId('evidence-toggle')[0])
    await waitFor(() => expect(screen.getByText('x-powered-by: php/8.1')).toBeTruthy())

    // 页面文本里不出现 IP（证据、关系字段都不带 IP）。
    expect(document.body.textContent).not.toMatch(/\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b/)
  })
})
