import React from 'react'
import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest'
import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'
import { App as AntApp } from 'antd'
import ServiceScan, { validServiceScanPattern as validPat } from './pages/ServiceScan'
import AppLayout from './layout/AppLayout'
import { HELP } from './help'

beforeAll(() => {
  window.matchMedia = window.matchMedia || ((q) => ({ matches: false, media: q, addListener() {}, removeListener() {}, addEventListener() {}, removeListener() {}, dispatchEvent() {} }))
  global.ResizeObserver = global.ResizeObserver || class { observe() {} unobserve() {} disconnect() {} }
})
afterEach(() => vi.restoreAllMocks())

describe('T49 扫描授权范围', () => {
  it('导航含扫描授权范围', () => {
    render(<MemoryRouter initialEntries={['/service-scan']}><AppLayout /></MemoryRouter>)
    expect(screen.getByText('扫描授权范围')).toBeTruthy()
  })

  it('默认关闭，可开启并添加范围', async () => {
    const calls = []
    let enabled = false
    let items = []
    global.fetch = vi.fn(async (url, opts) => {
      const u = String(url)
      const method = (opts?.method || 'GET').toUpperCase()
      calls.push(method + ' ' + u)
      let body = { enabled, items }
      let status = 200
      if (method === 'PATCH' && u.startsWith('/api/service-scan')) {
        const b = JSON.parse(opts.body)
        enabled = !!b.enabled
        body = { enabled, items }
      } else if (method === 'POST' && u.startsWith('/api/service-scan/scopes')) {
        const b = JSON.parse(opts.body)
        items = [{ id: 1, pattern: b.pattern.toLowerCase() }]
        body = items[0]
        status = 201
      }
      return { ok: status < 400, status, text: async () => JSON.stringify(body) }
    })
    render(<AntApp><ServiceScan /></AntApp>)
    await waitFor(() => expect(screen.getByTestId('service-scan-enabled')).toBeTruthy())
    const sw = screen.getByTestId('service-scan-enabled')
    expect(sw.getAttribute('aria-checked')).toBe('false')
    expect(screen.getByText(HELP.serviceScan)).toBeTruthy()
    fireEvent.click(sw)
    await waitFor(() => expect(calls.some((c) => c.startsWith('PATCH /api/service-scan'))).toBe(true))
    await waitFor(() => expect(sw.getAttribute('aria-checked')).toBe('true'))
    fireEvent.click(screen.getByText('添加范围'))
    fireEvent.change(screen.getByTestId('service-scan-pattern'), { target: { value: 'App.Example.TEST' } })
    fireEvent.click(screen.getByText('OK'))
    await waitFor(() => expect(calls.some((c) => c.startsWith('POST /api/service-scan/scopes'))).toBe(true))
    await waitFor(() => expect(screen.getByText('app.example.test')).toBeTruthy())
  })

  it('pattern 校验：域合法，IP 非法', () => {
    expect(validPat('example.test')).toBe(true)
    expect(validPat('*.example.test')).toBe(true)
    expect(validPat('not a host')).toBe(false)
    expect(validPat('127.0.0.1')).toBe(false)
    for (const p of ['127.1', '10.1', '192.168.1', '127.0.1', '0177.0.0.1', '0x7f.0.0.1', '1.2.3.4.5', '*.127.1', '*.0.1']) {
      expect(validPat(p), p).toBe(false)
    }
    expect(validPat('1.example.test')).toBe(true)
    expect(validPat('app.example.test')).toBe(true)
    // F2：末级不是纯数字的 inet_aton，只能靠 isInetAton 拦住
    for (const p of ['1.0x7f', '0x7f.1', '0x7f000001']) {
      expect(validPat(p), p).toBe(false)
    }
  })

  it('前端拦住 127.1，不发起添加请求', async () => {
    const calls = []
    global.fetch = vi.fn(async (url, opts) => {
      const u = String(url)
      const method = (opts?.method || 'GET').toUpperCase()
      calls.push(method + ' ' + u)
      return { ok: true, status: 200, text: async () => JSON.stringify({ enabled: false, items: [] }) }
    })
    render(<AntApp><ServiceScan /></AntApp>)
    await waitFor(() => expect(screen.getByTestId('service-scan-enabled')).toBeTruthy())
    fireEvent.click(screen.getByText('添加范围'))
    fireEvent.change(screen.getByTestId('service-scan-pattern'), { target: { value: '127.1' } })
    fireEvent.click(screen.getByText('OK'))
    await waitFor(() => expect(screen.getByText(/不能是 IP|example\.test/)).toBeTruthy())
    expect(calls.some((c) => c.startsWith('POST /api/service-scan/scopes'))).toBe(false)
  })
})
