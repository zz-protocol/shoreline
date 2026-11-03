import React from 'react'
import { describe, it, expect, vi, beforeAll } from 'vitest'
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { App as AntApp } from 'antd'
import Vendors from './pages/Vendors'

beforeAll(() => {
  window.matchMedia = window.matchMedia || ((q) => ({ matches: false, media: q, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent() {} }))
  global.ResizeObserver = global.ResizeObserver || class { observe() {} unobserve() {} disconnect() {} }
})

// T34：厂商行内添加根域（厂商预选）、错误就地显示、底部表单已删除
describe('T34 厂商行内添加根域', () => {
  it('在 VB 行内添加后立即在 VB 下看到；错误就地显示；无底部添加表单', async () => {
    const roots = { 1: [], 2: [{ id: 20, vendor_id: 2, domain: 'old.example.test' }] }
    const posts = []
    const ok = (b, status = 200) => ({ ok: status < 300, status, text: async () => JSON.stringify(b) })
    global.fetch = vi.fn((url, opts) => {
      const u = String(url)
      const m = u.match(/^\/api\/vendors\/(\d+)\/roots$/)
      if (opts?.method === 'POST' && m) {
        const body = JSON.parse(opts.body)
        posts.push({ vendor: Number(m[1]), ...body })
        if (body.domain === 'bad domain') return Promise.resolve(ok({ error: 'invalid domain: bad domain' }, 400))
        roots[m[1]].push({ id: 99, vendor_id: Number(m[1]), domain: body.domain })
        return Promise.resolve(ok({ id: 99 }, 201))
      }
      if (u === '/api/vendors' || u.startsWith('/api/vendors?')) return Promise.resolve(ok({ items: [{ id: 1, name: 'VA' }, { id: 2, name: 'VB' }] }))
      if (m) return Promise.resolve(ok({ items: roots[m[1]] }))
      return Promise.resolve(ok({ items: [] }))
    })
    render(<AntApp><Vendors /></AntApp>)
    await waitFor(() => expect(screen.getByText('VB')).toBeTruthy())
    expect(screen.queryByRole('dialog')).toBeNull()
    fireEvent.click(screen.getByTestId('add-root-2'))
    const box = await screen.findByTestId('inline-root-2')
    expect(within(box).getByText('为「VB」添加根域')).toBeTruthy()
    await waitFor(() => expect(within(box).getByText('old.example.test')).toBeTruthy())
    // 空值：本地校验就地显示
    fireEvent.click(screen.getByTestId('root-save-2'))
    await waitFor(() => expect(within(box).getByText(/请填写根域/)).toBeTruthy())
    // 服务端校验错误就地显示
    fireEvent.change(screen.getByTestId('root-input-2'), { target: { value: 'bad domain' } })
    fireEvent.click(screen.getByTestId('root-save-2'))
    await waitFor(() => expect(within(box).getByText(/invalid domain/)).toBeTruthy())
    // 成功：厂商预选为 VB，新根域立即出现在 VB 行内
    fireEvent.change(screen.getByTestId('root-input-2'), { target: { value: 'new.example.test' } })
    fireEvent.click(screen.getByTestId('root-save-2'))
    await waitFor(() => expect(within(screen.getByTestId('root-list-2')).getByText('new.example.test')).toBeTruthy())
    expect(posts.at(-1)).toEqual({ vendor: 2, domain: 'new.example.test' })
    expect(within(box).queryByText(/invalid domain/)).toBeNull()
    expect(screen.queryByTestId('inline-root-1')).toBeNull()
    expect(screen.queryByRole('dialog')).toBeNull()
  })
})
