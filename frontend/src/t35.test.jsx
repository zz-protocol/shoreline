import React from 'react'
import { describe, it, expect, vi, beforeAll } from 'vitest'
import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { App as AntApp } from 'antd'
import Scans from './pages/Scans'

beforeAll(() => {
  window.matchMedia = window.matchMedia || ((q) => ({ matches: false, media: q, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent() {} }))
  global.ResizeObserver = global.ResizeObserver || class { observe() {} unobserve() {} disconnect() {} }
})

// T35：终态可删（需确认）；pending/running/retrying 删除按钮置灰
describe('T35 扫描记录删除', () => {
  it('非终态置灰；终态经确认后 DELETE /api/scan-tasks/{id}', async () => {
    const dels = []
    const scans = ['pending', 'running', 'retrying', 'done', 'failed'].map((s, i) => ({ id: i + 1, kind: 'passive', status: s, root_id: 1, created_at: '2026-11-03T00:00:00Z' }))
    global.fetch = vi.fn((url, opts) => {
      const u = String(url)
      if (opts?.method === 'DELETE') { dels.push(u); return Promise.resolve({ ok: true, status: 204, text: async () => '' }) }
      if (u.startsWith('/api/scans')) return Promise.resolve({ ok: true, status: 200, text: async () => JSON.stringify({ items: scans }) })
      return Promise.resolve({ ok: true, status: 200, text: async () => '{"items":[]}' })
    })
    render(<AntApp><Scans /></AntApp>)
    await waitFor(() => expect(screen.getByTestId('del-scan-4')).toBeTruthy())
    for (const id of [1, 2, 3]) expect(screen.getByTestId(`del-scan-${id}`).disabled).toBe(true)
    for (const id of [4, 5]) expect(screen.getByTestId(`del-scan-${id}`).disabled).toBe(false)
    fireEvent.click(screen.getByTestId('del-scan-4'))
    expect(dels).toEqual([]) // 先弹确认
    await waitFor(() => expect(screen.getByText(/删除这条扫描记录/)).toBeTruthy())
    fireEvent.click(screen.getByRole('button', { name: /^(OK|确 ?定)$/ }))
    await waitFor(() => expect(dels).toEqual(['/api/scan-tasks/4']))
    await waitFor(() => expect(screen.queryByTestId('del-scan-4')).toBeNull())
  })
})
