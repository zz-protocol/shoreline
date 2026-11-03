import React from 'react'
import { describe, it, expect, vi, beforeAll } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import { readFileSync } from 'fs'
import { resolve } from 'path'
import { AgentGuide } from './pages/Docs'

beforeAll(() => {
  window.matchMedia = window.matchMedia || ((q) => ({ matches: false, media: q, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent() {} }))
})

// T36：/docs 页展示 Agent 使用说明（GET /api/agent-guide）
describe('T36 /docs Agent 使用说明', () => {
  it('从 /api/agent-guide 读取并完整展示', async () => {
    const md = readFileSync(resolve(__dirname, '../../docs/AGENT_GUIDE.md'), 'utf8')
    const urls = []
    global.fetch = vi.fn((u) => { urls.push(String(u)); return Promise.resolve({ ok: true, status: 200, text: async () => md }) })
    render(<AgentGuide />)
    await waitFor(() => expect(screen.getByTestId('agent-guide').textContent).toBe(md))
    expect(urls).toEqual(['/api/agent-guide'])
    expect(md).toContain('/api/mine/claim')
    expect(md).toContain('system prompt')
  })
})
