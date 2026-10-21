import React from 'react'
import { describe, it, expect, vi, beforeAll } from 'vitest'
import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { App as AntApp } from 'antd'
import Vendors from './pages/Vendors'

beforeAll(() => {
  window.matchMedia = window.matchMedia || ((q) => ({ matches: false, media: q, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent() {} }))
  global.ResizeObserver = global.ResizeObserver || class { observe() {} unobserve() {} disconnect() {} }
})

// T23 打回③：切换厂商清空 roots，未加载完按钮禁用；旧厂商迟到的 roots 响应丢弃，触发的是当前厂商的根域
describe('Vendors 切换厂商（延迟 mock）', () => {
  it('切换后 roots 清空、按钮禁用；迟到的旧 roots 不覆盖；POST 用新厂商根域', async () => {
    const rootsWait = {}
    const posts = []
    const ok = (b) => ({ ok: true, status: 200, text: async () => JSON.stringify(b) })
    global.fetch = vi.fn((url, opts) => {
      const u = String(url)
      if (opts?.method === 'POST') { posts.push(JSON.parse(opts.body)); return Promise.resolve({ ok: true, status: 202, text: async () => '{"id":9}' }) }
      if (u === '/api/vendors' || u.startsWith('/api/vendors?')) return Promise.resolve(ok({ items: [{ id: 1, name: 'VA', company_name: 'A' }, { id: 2, name: 'VB', company_name: 'B' }] }))
      const m = u.match(/^\/api\/vendors\/(\d+)\/roots/)
      if (m) return new Promise((r) => { rootsWait[m[1]] = () => r(ok({ items: [{ id: Number(m[1]) * 10, vendor_id: Number(m[1]), domain: `r${m[1]}.example.test` }] })) })
      return Promise.resolve(ok({ items: [] }))
    })
    render(<AntApp><Vendors /></AntApp>)
    await waitFor(() => expect(screen.getByText('VA')).toBeTruthy())
    fireEvent.click(screen.getByText('VA'))
    await waitFor(() => expect(rootsWait['1']).toBeTruthy())
    rootsWait['1']()
    await waitFor(() => expect(screen.getByTestId('expand-btn').disabled).toBe(false))
    // 再点 VA 触发一次 roots 请求（延迟），随即切到 VB
    fireEvent.click(screen.getByText('VA'))
    const lateA = rootsWait['1']
    fireEvent.click(screen.getByText('VB'))
    await waitFor(() => expect(screen.getByTestId('expand-btn').disabled).toBe(true)) // roots 已清空、加载中
    lateA() // 旧厂商迟到
    await new Promise((r) => setTimeout(r, 30))
    expect(screen.getByTestId('expand-btn').disabled).toBe(true)
    rootsWait['2']()
    await waitFor(() => expect(screen.getByTestId('expand-btn').disabled).toBe(false))
    fireEvent.click(screen.getByTestId('expand-btn'))
    await waitFor(() => expect(posts).toEqual([{ root_id: 20, kind: 'expand' }]))
  })
})
