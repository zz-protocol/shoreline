import { describe, it, expect } from 'vitest'
import { maskPreview } from './pages/ProbeKeys'

describe('T30 maskPreview', () => {
  it('只露后 4 位；短 key 全掩', () => {
    expect(maskPreview('')).toBe('')
    expect(maskPreview('ab')).toBe('****')
    expect(maskPreview('abcdefgh')).toBe('****efgh')
    expect(maskPreview('  plain-fofa-key-ABCD  ')).toBe('****ABCD')
  })
})
