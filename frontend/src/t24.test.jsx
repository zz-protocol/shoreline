import { describe, expect, it } from 'vitest'
import { filterParams } from './eventsWindow'
import { HELP } from './help'

describe('T24 客户托管', () => {
  it('播报默认不带 include_customer_hosted，打开开关才带', () => {
    expect(filterParams({ level: 'asset' }).has('include_customer_hosted')).toBe(false)
    expect(filterParams({ level: 'asset', hosted: true }).get('include_customer_hosted')).toBe('true')
  })
  it('有中文说明', () => {
    expect(HELP.hosted).toContain('客户托管')
  })
})

import React from 'react'
import { App as AntApp } from 'antd'
import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { vi, beforeAll, afterEach } from 'vitest'
import { cleanup } from '@testing-library/react'
import HostedRules from './pages/HostedRules'

beforeAll(() => {
  window.matchMedia = window.matchMedia || ((q) => ({ matches: false, media: q, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent() {} }))
})
afterEach(() => cleanup())

describe('客户托管规则页：添加失败', () => {
  const setup = (postStatus, postBody) => {
    global.fetch = vi.fn(async (url, opts) => {
      if (opts?.method === 'POST') return { ok: false, status: postStatus, text: async () => JSON.stringify(postBody) }
      return { ok: true, status: 200, text: async () => JSON.stringify({ items: [] }) }
    })
  }
  const errors = []
  window.addEventListener('error', (e) => errors.push(e))
  window.addEventListener('unhandledrejection', (e) => errors.push(e))

  it('400：弹窗保持打开、保留输入、显示错误', async () => {
    setup(400, { error: 'invalid pattern: 通配符之后的固定后缀「com」是公共后缀' })
    render(<AntApp><HostedRules /></AntApp>)
    fireEvent.click(screen.getByText('添加规则'))
    const input = await screen.findByPlaceholderText('*.example-cloud.com')
    fireEvent.change(input, { target: { value: '*.*.com' } })
    fireEvent.click(screen.getByText('OK'))
    await waitFor(() => expect(screen.getByTestId('add-error').textContent).toContain('公共后缀'))
    expect(screen.getByPlaceholderText('*.example-cloud.com').value).toBe('*.*.com')
  })

  it('409：同样保持打开', async () => {
    setup(409, { error: 'pattern already exists' })
    render(<AntApp><HostedRules /></AntApp>)
    fireEvent.click(screen.getAllByText('添加规则')[0])
    const input = (await screen.findAllByPlaceholderText('*.example-cloud.com')).at(-1)
    fireEvent.change(input, { target: { value: '*.github.io' } })
    fireEvent.click(screen.getAllByText('OK').at(-1))
    await waitFor(() => expect(screen.getAllByTestId('add-error').at(-1).textContent).toContain('already exists'))
  })

  it('表单校验失败不抛未处理 reject', async () => {
    setup(400, {})
    render(<AntApp><HostedRules /></AntApp>)
    fireEvent.click(screen.getAllByText('添加规则')[0])
    await screen.findAllByPlaceholderText('*.example-cloud.com', {}, { timeout: 10000 })
    fireEvent.click(screen.getAllByText('OK').at(-1)) // 空输入
    await waitFor(() => expect(screen.getAllByText('请填写规则').length).toBeGreaterThan(0), { timeout: 10000 })
    await new Promise((r) => setTimeout(r, 50))
    expect(errors.length).toBe(0)
    expect(global.fetch.mock.calls.some(([, o]) => o?.method === 'POST')).toBe(false)
  }, 20000)
})
