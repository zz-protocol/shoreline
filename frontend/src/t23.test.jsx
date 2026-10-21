import React from 'react'
import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest'
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import ExpandPanel from './pages/ExpandPanel'

beforeAll(() => {
  window.matchMedia = window.matchMedia || ((q) => ({ matches: false, media: q, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent() {} }))
})
afterEach(() => cleanup())

const scan = (status, result) => ({ id: 9, kind: 'expand', status, result, degraded: !!result?.degraded, degraded_reason: result?.degraded_reason, created_at: '2026-10-21T00:00:00Z' })

describe('T23 一键 expand', () => {
  it('点击触发 → 运行中轮询 → 完成显示结果并回调刷新待确认', async () => {
    const seq = [null, scan('running'), scan('done', { found: 4, new_candidates: 2 })]
    let i = 0
    const posts = []
    global.fetch = vi.fn(async (url, opts) => {
      if (opts?.method === 'POST') {
        posts.push(JSON.parse(opts.body))
        i = 1
        return { ok: true, status: 201, text: async () => JSON.stringify({ id: 9 }) }
      }
      const s = seq[Math.min(i, 2)]
      if (i >= 1) i++
      return { ok: true, status: 200, text: async () => JSON.stringify({ items: s ? [s] : [] }) }
    })
    const onDone = vi.fn()
    render(<ExpandPanel vendor={{ id: 3, company_name: '示例科技有限公司' }} roots={[{ id: 7, domain: 'a.example.test' }]} onDone={onDone} interval={20} />)
    await waitFor(() => expect(screen.getByText('还没有跑过 expand')).toBeTruthy())
    expect(String(global.fetch.mock.calls[0][0])).toContain('/api/scans?vendor=3&kind=expand&limit=1')
    fireEvent.click(screen.getByTestId('expand-btn'))
    await waitFor(() => expect(posts).toEqual([{ root_id: 7, kind: 'expand' }]))
    await waitFor(() => expect(screen.getByText('运行中')).toBeTruthy())
    expect(screen.getByTestId('expand-btn').disabled).toBe(true)
    await waitFor(() => expect(screen.getByText('发现 4 个，新增待确认 2 个')).toBeTruthy())
    expect(onDone).toHaveBeenCalledTimes(1)
    expect(screen.getByTestId('expand-btn').disabled).toBe(false)
  })

  it('没有根域：按钮禁用并提示；降级显示原因', async () => {
    global.fetch = vi.fn(async () => ({ ok: true, status: 200, text: async () => JSON.stringify({ items: [scan('done', { found: 0, new_candidates: 0, degraded: true, degraded_reason: 'icp: timeout' })] }) }))
    render(<ExpandPanel vendor={{ id: 3 }} roots={[]} />)
    await waitFor(() => expect(screen.getByText('降级：icp: timeout')).toBeTruthy())
    expect(screen.getByTestId('expand-btn').disabled).toBe(true)
    expect(screen.getByText(/请先添加根域/)).toBeTruthy()
  })

  it('触发失败（如 409）显示错误', async () => {
    global.fetch = vi.fn(async (url, opts) => (opts?.method === 'POST'
      ? { ok: false, status: 409, text: async () => JSON.stringify({ error: 'conflict' }) }
      : { ok: true, status: 200, text: async () => JSON.stringify({ items: [] }) }))
    render(<ExpandPanel vendor={{ id: 3 }} roots={[{ id: 7 }]} />)
    await waitFor(() => expect(screen.getByText('还没有跑过 expand')).toBeTruthy())
    fireEvent.click(screen.getByTestId('expand-btn'))
    await waitFor(() => expect(screen.getByText('conflict')).toBeTruthy())
  })
})
