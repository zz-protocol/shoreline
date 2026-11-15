import React from 'react'
import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import { App as AntApp } from 'antd'
import Assets from './pages/Assets'
import { HELP } from './help'

beforeAll(() => {
  window.matchMedia = window.matchMedia || ((q) => ({ matches: false, media: q, addListener() {}, removeListener() {}, addEventListener() {}, removeListener() {}, dispatchEvent() {} }))
  global.ResizeObserver = global.ResizeObserver || class { observe() {} unobserve() {} disconnect() {} }
})
afterEach(() => vi.restoreAllMocks())

describe('T50 CDN 前置标记', () => {
  it('展示 cdn_fronted 标签，不展示 IP', async () => {
    global.fetch = vi.fn(async (url) => {
      const u = String(url)
      let body = { items: [] }
      if (u.startsWith('/api/vendors')) body = { items: [] }
      else if (u.startsWith('/api/assets?')) {
        body = {
          items: [{
            id: 1,
            subdomain: 'cdn-cname.example.test',
            url: 'https://cdn-cname.example.test:443',
            cdn: 'cloudflare',
            cdn_fronted: true,
            ip: '192.0.2.80',
            ips: ['192.0.2.80'],
            in_scope: true,
            wildcard: false,
            customer_hosted: false,
            alive: 'alive',
            sources: ['dns'],
          }, {
            id: 2,
            subdomain: 'origin.example.test',
            url: 'https://origin.example.test:443',
            cdn_fronted: false,
            in_scope: true,
            wildcard: false,
            customer_hosted: false,
            alive: 'alive',
            sources: ['dns'],
          }],
        }
      }
      return { ok: true, status: 200, text: async () => JSON.stringify(body) }
    })
    render(<AntApp><Assets /></AntApp>)
    await waitFor(() => expect(screen.getByText('cdn-cname.example.test')).toBeTruthy())
    expect(screen.getByTestId('cdn-fronted-tag').textContent).toBe('cloudflare')
    expect(screen.getByText('origin.example.test')).toBeTruthy()
    expect(screen.queryByText('192.0.2.80')).toBeNull()
    expect(screen.queryByText('IP')).toBeNull()
    expect(HELP.cdnFronted).toMatch(/不入库/)
  })
})
