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

describe('T44 冷门分', () => {
  it('界面注明公开披露数仅供筛选；可筛上限/未关联仓库并填写仓库', async () => {
    const urls = []
    const patches = []
    global.fetch = vi.fn(async (url, opts) => {
      const u = String(url)
      urls.push(u)
      const method = (opts?.method || 'GET').toUpperCase()
      let body = { items: [] }
      if (u.startsWith('/api/vendors')) body = { items: [] }
      else if (method === 'PATCH' && u.startsWith('/api/components/10')) {
        const req = JSON.parse(opts.body)
        patches.push(req)
        body = { id: 10, name: 'RareCMS', github_repo: req.github_repo, stars: 42, advisory_count: 0, enrich_error: null }
      } else if (u.startsWith('/api/components?') && !u.includes('/assets') && !u.includes('/values')) {
        body = { items: [
          { id: 10, name: 'RareCMS', source: 'wappalyzer', url_count: 2, asset_count: 2, vendor_count: 2, is_common: false, github_repo: null, stars: null, advisory_count: 0 },
        ], next_cursor: null }
      } else if (u.startsWith('/api/components/10/assets')) {
        body = { items: [] }
      }
      return { ok: true, status: 200, text: async () => JSON.stringify(body) }
    })
    render(<AntApp><Components /></AntApp>)
    await waitFor(() => expect(screen.getByText('RareCMS')).toBeTruthy())
    const note = screen.getByTestId('advisory-note')
    expect(note.textContent).toBe(HELP.advisories)
    expect(note.textContent).toMatch(/仅供筛选/)
    expect(note.textContent).toMatch(/不代表该组件安全/)
    expect(screen.getByText('Star')).toBeTruthy()
    expect(screen.getByText('公开披露数')).toBeTruthy()
    expect(screen.getByText('GitHub 仓库')).toBeTruthy()

    fireEvent.change(screen.getByTestId('max-stars'), { target: { value: '100' } })
    await waitFor(() => expect(urls.some((u) => u.includes('max_stars=100'))).toBe(true))
    fireEvent.click(screen.getByTestId('missing-repo'))
    await waitFor(() => expect(urls.some((u) => u.includes('missing_repo=true'))).toBe(true))

    fireEvent.click(screen.getByText('RareCMS'))
    await waitFor(() => expect(screen.getByTestId('github-repo')).toBeTruthy())
    fireEvent.change(screen.getByTestId('github-repo'), { target: { value: 'acme/rarecms' } })
    fireEvent.click(screen.getByTestId('save-repo'))
    await waitFor(() => expect(patches).toEqual([{ github_repo: 'acme/rarecms' }]))
  })
})
