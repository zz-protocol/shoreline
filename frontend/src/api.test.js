import { describe, it, expect, beforeEach, vi } from 'vitest'
import { getToken, setToken, api } from './api'

describe('auth token', () => {
  beforeEach(() => {
    localStorage.clear()
  })
  it('stores and clears token', () => {
    expect(getToken()).toBe('')
    setToken('sam_x')
    expect(getToken()).toBe('sam_x')
    setToken('')
    expect(getToken()).toBe('')
  })
})

describe('api client', () => {
  beforeEach(() => {
    localStorage.clear()
    setToken('tok')
  })
  it('sends bearer and parses json', async () => {
    global.fetch = vi.fn(async () => ({
      ok: true,
      status: 200,
      text: async () => '{"items":[1]}',
    }))
    const out = await api('GET', '/api/vendors')
    expect(out.items).toEqual([1])
    expect(fetch).toHaveBeenCalledWith('/api/vendors', expect.objectContaining({
      method: 'GET',
      headers: expect.objectContaining({ Authorization: 'Bearer tok' }),
    }))
  })
  it('throws with error message on 4xx', async () => {
    global.fetch = vi.fn(async () => ({
      ok: false,
      status: 409,
      statusText: 'Conflict',
      text: async () => '{"error":"vendor/root has pending or running scans"}',
    }))
    await expect(api('DELETE', '/api/vendors/1')).rejects.toThrow(/pending or running/)
  })
  it('handles 204', async () => {
    global.fetch = vi.fn(async () => ({ ok: true, status: 204, text: async () => '' }))
    expect(await api('DELETE', '/api/tokens/1')).toBeNull()
  })
})
