import React from 'react'
import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import { App as AntApp } from 'antd'
import ServiceScan, { serviceResultStatus } from './pages/ServiceScan'

beforeAll(() => {
  window.matchMedia = window.matchMedia || ((q) => ({ matches: false, media: q, addListener() {}, removeListener() {}, addEventListener() {}, removeListener() {}, dispatchEvent() {} }))
  global.ResizeObserver = global.ResizeObserver || class { observe() {} unobserve() {} disconnect() {} }
})
afterEach(() => vi.restoreAllMocks())

describe('T55 服务探测三态', () => {
  it('状态分档：已验证 / 仅 TCP 可达 / 反代/CDN', () => {
    expect(serviceResultStatus({ open: true, verified: true, response_kind: 'banner' }).label).toBe('已验证')
    expect(serviceResultStatus({ open: true, verified: true, response_kind: 'probe' }).label).toBe('已验证')
    expect(serviceResultStatus({ open: true, verified: false, response_kind: 'none' }).label).toBe('仅 TCP 可达')
    expect(serviceResultStatus({ open: true, verified: false, response_kind: 'reverse_proxy' }).label).toBe('反代/CDN')
    expect(serviceResultStatus({ open: false, verified: false, response_kind: 'none' }).label).toBe('关闭')
  })

  it('开放数只计已验证，同时展示 TCP 可达', async () => {
    global.fetch = vi.fn(async (url) => {
      const u = String(url)
      let body = { enabled: true, items: [] }
      if (u.startsWith('/api/service-results')) {
        body = {
          stats: { open: 2, verified: 2, tcp_reachable: 4, named: 2, reverse_proxy: 1, total: 4 },
          items: [
            { id: 1, host: 'db.example.test', port: 3306, open: true, verified: true, response_kind: 'banner', name: 'MySQL', version: '8.0.36' },
            { id: 2, host: 'cache.example.test', port: 6379, open: true, verified: false, response_kind: 'none', name: '', version: '' },
            { id: 3, host: 'edge.example.test', port: 443, open: true, verified: false, response_kind: 'reverse_proxy', name: '', version: '' },
          ],
          next_cursor: null,
        }
      }
      return { ok: true, status: 200, text: async () => JSON.stringify(body) }
    })
    render(<AntApp><ServiceScan /></AntApp>)
    await waitFor(() => expect(screen.getByTestId('service-results')).toBeTruthy())
    expect(screen.getByTestId('stat-verified').textContent).toBe('2')
    expect(screen.getByTestId('stat-tcp').textContent).toBe('4')
    expect(screen.getByTestId('stat-named').textContent).toBe('2')
    expect(screen.getByTestId('stat-reverse-proxy').textContent).toBe('1')
    await waitFor(() => expect(screen.getByTestId('service-state-1').textContent).toBe('已验证'))
    expect(screen.getByTestId('service-state-2').textContent).toBe('仅 TCP 可达')
    expect(screen.getByTestId('service-state-3').textContent).toBe('反代/CDN')
    expect(screen.queryByText(/banner 原文/)).toBeTruthy()
  })
})
