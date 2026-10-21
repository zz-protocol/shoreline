import React from 'react'
import { describe, it, expect, vi, beforeAll } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import { App as AntApp } from 'antd'
import { HELP } from './help'
import Vendors from './pages/Vendors'
import Scans from './pages/Scans'

beforeAll(() => {
  window.matchMedia = window.matchMedia || ((q) => ({ matches: false, media: q, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent() {} }))
  const gcs = window.getComputedStyle
  window.getComputedStyle = (el) => gcs(el) // jsdom 不支持 pseudoElt 参数
  global.ResizeObserver = global.ResizeObserver || class { observe() {} unobserve() {} disconnect() {} }
})

const emptyFetch = () => vi.fn(async () => ({ ok: true, status: 200, text: async () => '{"items":[],"next_cursor":null}' }))

describe('T22 中文说明与空状态引导', () => {
  it('每种扫描类型和关键名词都有中文说明', () => {
    for (const k of ['passive', 'active', 'expand', 'dns', 'http']) expect(HELP.kinds[k]).toMatch(/[\u4e00-\u9fa5]/)
    for (const k of ['root', 'scope', 'candidate']) expect(HELP[k].length).toBeGreaterThan(10)
  })

  it('厂商页空状态给出下一步引导', async () => {
    global.fetch = emptyFetch()
    render(<AntApp><Vendors /></AntApp>)
    await waitFor(() => expect(screen.getByText('还没有厂商')).toBeTruthy())
    expect(screen.getByText(/新建厂商/, { selector: 'span.ant-typography' })).toBeTruthy()
  })

  it('扫描页空状态给出引导', async () => {
    global.fetch = emptyFetch()
    render(<AntApp><Scans /></AntApp>)
    await waitFor(() => expect(screen.getByText('还没有扫描任务')).toBeTruthy())
    expect(screen.getByText(/先跑 passive/)).toBeTruthy()
  })
})
