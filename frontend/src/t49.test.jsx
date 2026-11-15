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
  })
})
