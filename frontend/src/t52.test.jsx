import React from 'react'
import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest'
import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { App as AntApp } from 'antd'
import Components from './pages/Components'

beforeAll(() => {
  window.matchMedia = window.matchMedia || ((q) => ({ matches: false, media: q, addListener() {}, removeListener() {}, addEventListener() {}, removeListener() {}, dispatchEvent() {} }))
  global.ResizeObserver = global.ResizeObserver || class { observe() {} unobserve() {} disconnect() {} }
})
afterEach(() => vi.restoreAllMocks())

describe('T52 服务指纹待确认候选', () => {
  it('LLM 未配置时仍展示 service 未识别候选，确认后请求 POST confirm', async () => {
    const posts = []
    global.fetch = vi.fn(async (url, opts) => {
      const u = String(url)
      const method = (opts?.method || 'GET').toUpperCase()
      let body = { items: [] }
      if (u.startsWith('/api/vendors')) body = { items: [] }
      else if (u.startsWith('/api/component-candidates')) {
        body = posts.some((p) => p.includes('/confirm'))
          ? { configured: false, items: [] }
          : { configured: false, items: [{ id: 77, name: 'unidentified-service-abcd1234ef56', reason: 'weird-banner-no-rule', url_count: 0, source: 'service', confirmed: false }] }
      } else if (method === 'POST' && u.includes('/api/components/77/confirm')) {
        posts.push(u)
        body = { id: 77, name: 'unidentified-service-abcd1234ef56', confirmed: true }
      } else if (u.startsWith('/api/components?')) {
        body = { items: [], next_cursor: null }
      }
      return { ok: true, status: 200, text: async () => JSON.stringify(body) }
    })
    render(<AntApp><Components /></AntApp>)
    await waitFor(() => expect(screen.getByTestId('llm-candidates')).toBeTruthy())
    expect(screen.getByText('unidentified-service-abcd1234ef56')).toBeTruthy()
    fireEvent.click(screen.getByTestId('confirm-cand-77'))
    await waitFor(() => expect(posts).toEqual(['/api/components/77/confirm']))
  })
})
