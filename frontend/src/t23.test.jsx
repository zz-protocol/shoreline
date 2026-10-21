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
    // 后端状态由测试显式推进（不靠计时），避免「运行中」只存在一个轮询周期而被 waitFor 错过
    let state = null
    const posts = []
    global.fetch = vi.fn(async (url, opts) => {
      if (opts?.method === 'POST') {
        posts.push(JSON.parse(opts.body))
        state = scan('running')
        return { ok: true, status: 201, text: async () => JSON.stringify({ id: 9 }) }
      }
      return { ok: true, status: 200, text: async () => JSON.stringify({ items: state ? [state] : [] }) }
    })
    const onDone = vi.fn()
    render(<ExpandPanel vendor={{ id: 3, company_name: '示例科技有限公司' }} roots={[{ id: 7, domain: 'a.example.test' }]} onDone={onDone} interval={20} />)
    await waitFor(() => expect(screen.getByText('还没有跑过 expand')).toBeTruthy())
    expect(String(global.fetch.mock.calls[0][0])).toContain('/api/scans?vendor=3&kind=expand&limit=1')
    fireEvent.click(screen.getByTestId('expand-btn'))
    await waitFor(() => expect(posts).toEqual([{ root_id: 7, kind: 'expand' }]))
    await waitFor(() => expect(screen.getByText('运行中')).toBeTruthy())
    expect(screen.getByTestId('expand-btn').disabled).toBe(true)
    const polls = global.fetch.mock.calls.length
    await waitFor(() => expect(global.fetch.mock.calls.length).toBeGreaterThan(polls + 1)) // 运行中持续轮询
    expect(onDone).not.toHaveBeenCalled()
    state = scan('done', { found: 4, new_candidates: 2 })
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

  it('409：中文提示，并拉取正在跑的任务显示其状态；其它错误原样显示', async () => {
    let gets = 0
    global.fetch = vi.fn(async (url, opts) => {
      if (opts?.method === 'POST') return { ok: false, status: 409, text: async () => JSON.stringify({ error: 'expand already in progress for this vendor (scan 9)' }) }
      gets++
      return { ok: true, status: 200, text: async () => JSON.stringify({ items: gets > 1 ? [scan('running')] : [] }) }
    })
    render(<ExpandPanel vendor={{ id: 3 }} roots={[{ id: 7, vendor_id: 3 }]} interval={60000} />)
    await waitFor(() => expect(screen.getByText('还没有跑过 expand')).toBeTruthy())
    fireEvent.click(screen.getByTestId('expand-btn'))
    await waitFor(() => expect(screen.getByText('该厂商已有 expand 在进行')).toBeTruthy())
    await waitFor(() => expect(screen.getByText('运行中')).toBeTruthy())
    expect(screen.getByText(/#9/)).toBeTruthy()
    cleanup()
    global.fetch = vi.fn(async (url, opts) => (opts?.method === 'POST'
      ? { ok: false, status: 500, text: async () => JSON.stringify({ error: 'boom' }) }
      : { ok: true, status: 200, text: async () => JSON.stringify({ items: [] }) }))
    render(<ExpandPanel vendor={{ id: 3 }} roots={[{ id: 7 }]} />)
    await waitFor(() => expect(screen.getByText('还没有跑过 expand')).toBeTruthy())
    fireEvent.click(screen.getByTestId('expand-btn'))
    await waitFor(() => expect(screen.getByText('boom')).toBeTruthy())
  })

  it('连点三次只发 1 个 POST（ref 同步守卫）', async () => {
    let release
    const posts = []
    global.fetch = vi.fn((url, opts) => {
      if (opts?.method === 'POST') {
        posts.push(opts.body)
        return new Promise((r) => { release = () => r({ ok: true, status: 202, text: async () => JSON.stringify({ id: 9 }) }) })
      }
      return Promise.resolve({ ok: true, status: 200, text: async () => JSON.stringify({ items: [] }) })
    })
    render(<ExpandPanel vendor={{ id: 3 }} roots={[{ id: 7, vendor_id: 3 }]} interval={60000} />)
    await waitFor(() => expect(screen.getByText('还没有跑过 expand')).toBeTruthy())
    const btn = screen.getByTestId('expand-btn')
    fireEvent.click(btn); fireEvent.click(btn); fireEvent.click(btn)
    expect(posts.length).toBe(1)
    release()
    await waitFor(() => expect(screen.getByTestId('expand-btn').disabled).toBe(false))
    expect(posts.length).toBe(1)
  })

  it('切换厂商：上一个厂商迟到的 /api/scans 响应被丢弃（延迟 mock）', async () => {
    const pending = {}
    global.fetch = vi.fn((url) => new Promise((r) => {
      const v = new URL(String(url), 'http://x').searchParams.get('vendor')
      pending[v] = (items) => r({ ok: true, status: 200, text: async () => JSON.stringify({ items }) })
    }))
    const { rerender } = render(<ExpandPanel vendor={{ id: 1 }} roots={[{ id: 11, vendor_id: 1 }]} />)
    await waitFor(() => expect(pending['1']).toBeTruthy())
    rerender(<ExpandPanel vendor={{ id: 2 }} roots={[{ id: 22, vendor_id: 2 }]} />)
    await waitFor(() => expect(pending['2']).toBeTruthy())
    pending['2']([])
    await waitFor(() => expect(screen.getByText('还没有跑过 expand')).toBeTruthy())
    pending['1']([{ ...scan('done', { found: 1, new_candidates: 1 }), id: 101 }]) // 厂商 1 的迟到响应
    await new Promise((r) => setTimeout(r, 50))
    expect(screen.queryByText(/#101/)).toBeNull()
    expect(screen.getByText('还没有跑过 expand')).toBeTruthy()
  })

  it('roots 未加载（null）时按钮禁用；只触发属于当前厂商的根域', async () => {
    const posts = []
    global.fetch = vi.fn(async (url, opts) => {
      if (opts?.method === 'POST') { posts.push(JSON.parse(opts.body)); return { ok: true, status: 202, text: async () => '{"id":9}' } }
      return { ok: true, status: 200, text: async () => JSON.stringify({ items: [] }) }
    })
    const { rerender } = render(<ExpandPanel vendor={{ id: 2 }} roots={null} />)
    await waitFor(() => expect(screen.getByText('还没有跑过 expand')).toBeTruthy())
    expect(screen.getByTestId('expand-btn').disabled).toBe(true)
    rerender(<ExpandPanel vendor={{ id: 2 }} roots={[{ id: 11, vendor_id: 1 }]} />) // 旧厂商的根域
    expect(screen.getByTestId('expand-btn').disabled).toBe(true)
    rerender(<ExpandPanel vendor={{ id: 2 }} roots={[{ id: 11, vendor_id: 1 }, { id: 22, vendor_id: 2 }]} />)
    fireEvent.click(screen.getByTestId('expand-btn'))
    await waitFor(() => expect(posts).toEqual([{ root_id: 22, kind: 'expand' }]))
  })
})
