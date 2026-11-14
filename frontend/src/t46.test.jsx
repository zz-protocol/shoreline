import React from 'react'
import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest'
import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { App as AntApp } from 'antd'
import Components from './pages/Components'
import { HELP } from './help'

beforeAll(() => {
  window.matchMedia = window.matchMedia || ((q) => ({ matches: false, media: q, addListener() {}, removeListener() {}, addEventListener() {}, removeListener() {}, dispatchEvent() {} }))
  global.ResizeObserver = global.ResizeObserver || class { observe() {} unobserve() {} disconnect() {} }
})
afterEach(() => vi.restoreAllMocks())

describe('T46 可选 LLM 候选', () => {
  it('未配置时提示关闭', async () => {
    global.fetch = vi.fn(async (url) => {
      const u = String(url)
      let body = { items: [] }
      if (u.startsWith('/api/component-candidates')) body = { configured: false, items: [] }
      return { ok: true, status: 200, text: async () => JSON.stringify(body) }
    })
    render(<AntApp><Components /></AntApp>)
    await waitFor(() => expect(screen.getByTestId('llm-unconfigured')).toBeTruthy())
    expect(screen.getByTestId('llm-unconfigured').textContent).toBe(HELP.llmCandidates)
    expect(screen.queryByTestId('llm-candidates')).toBeNull()
  })

  it('已配置时展示待确认候选，确认后请求 POST confirm', async () => {
    const posts = []
    global.fetch = vi.fn(async (url, opts) => {
      const u = String(url)
      const method = (opts?.method || 'GET').toUpperCase()
      let body = { items: [] }
      if (u.startsWith('/api/vendors')) body = { items: [] }
      else if (u.startsWith('/api/component-candidates')) {
        body = posts.some((p) => p.includes('/confirm'))
          ? { configured: true, items: [] }
          : { configured: true, items: [{ id: 99, name: 'RareLLMApp', reason: 'title looks custom', github_repo: 'acme/rare-llm-app', url_count: 1, source: 'llm', confirmed: false }] }
      } else if (method === 'POST' && u.includes('/api/components/99/confirm')) {
        posts.push(u)
        body = { id: 99, name: 'RareLLMApp', confirmed: true }
      } else if (u.startsWith('/api/components?')) {
        body = { items: [], next_cursor: null }
      }
      return { ok: true, status: 200, text: async () => JSON.stringify(body) }
    })
    render(<AntApp><Components /></AntApp>)
    await waitFor(() => expect(screen.getByTestId('llm-candidates')).toBeTruthy())
    expect(screen.getByText('RareLLMApp')).toBeTruthy()
    expect(screen.queryByTestId('llm-unconfigured')).toBeNull()
    fireEvent.click(screen.getByTestId('confirm-cand-99'))
    await waitFor(() => expect(posts).toEqual(['/api/components/99/confirm']))
  })
})
