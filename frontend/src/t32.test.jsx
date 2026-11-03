import React from 'react'
import { describe, expect, it, vi, beforeAll, afterEach } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import { App as AntApp } from 'antd'
import { assetFilterParams, MINE_STATUS } from './pages/Assets'
import Assets from './pages/Assets'
import { HELP } from './help'

beforeAll(() => {
  window.matchMedia = window.matchMedia || ((q) => ({ matches: false, media: q, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent() {} }))
  global.ResizeObserver = global.ResizeObserver || class { observe() {} unobserve() {} disconnect() {} }
})
afterEach(() => vi.restoreAllMocks())

describe('T32 挖掘状态', () => {
  it('assetFilterParams 带 mine_status；文案含冷却与 TTL', () => {
    expect(assetFilterParams({ mineStatus: 'claimed' }).get('mine_status')).toBe('claimed')
    expect(assetFilterParams({}).has('mine_status')).toBe(false)
    expect(HELP.mine).toMatch(/MINE_CLAIM_TTL_HOURS/)
    expect(HELP.mine).toMatch(/MINE_COOLDOWN_DAYS/)
    expect(Object.keys(MINE_STATUS)).toEqual(['unset', 'claimed', 'done', 'deferred', 'abandoned'])
  })
  it('资产页有挖掘筛选、认领人/最近挖掘列，并展示状态标签与认领人', async () => {
    global.fetch = vi.fn(async (url) => {
      const u = String(url)
      let body
      if (u.startsWith('/api/vendors')) body = { items: [] }
      else if (u.startsWith('/api/assets?')) {
        body = {
          items: [{
            id: 9, subdomain: 'x.example.test', url: '',
            alive: 'alive', in_scope: true, wildcard: false, customer_hosted: false, sources: [],
            mine_status: 'claimed', mine_claimed_by: 'agent-z',
            mine_claimed_at: '2026-11-03T01:00:00Z', mine_completed_at: null,
          }],
          next_cursor: null,
        }
      } else body = { items: [] }
      return { ok: true, status: 200, text: async () => JSON.stringify(body) }
    })
    render(<AntApp><Assets /></AntApp>)
    await waitFor(() => expect(screen.getByTestId('mine-status')).toBeTruthy())
    expect(screen.getByText(/MINE_CLAIM_TTL_HOURS/)).toBeTruthy()
    expect(screen.getByText('认领人')).toBeTruthy()
    expect(screen.getByText('最近挖掘')).toBeTruthy()
    await waitFor(() => expect(screen.getByTestId('mine-tag').textContent).toBe('挖掘中'))
    expect(screen.getByText('agent-z')).toBeTruthy()
  })
})
