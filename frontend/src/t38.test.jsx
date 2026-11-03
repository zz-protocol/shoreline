import React from 'react'
import { describe, expect, it } from 'vitest'
import { render } from '@testing-library/react'
import { ALIVE_FILTER, AliveTag, assetFilterParams } from './pages/Assets'

describe('T38 dead_reason UI', () => {
  it('ALIVE_FILTER 映射到 alive + dead_reason', () => {
    const by = Object.fromEntries(ALIVE_FILTER.map((o) => [o.value, o]))
    expect(by.nxdomain).toMatchObject({ alive: 'dead', deadReason: 'nxdomain' })
    expect(by.no_http).toMatchObject({ alive: 'dead', deadReason: 'no_http' })
    expect(assetFilterParams({ alive: by.nxdomain.alive, deadReason: by.nxdomain.deadReason }).toString())
      .toContain('dead_reason=nxdomain')
  })
  it('AliveTag 按 dead_reason 展示', () => {
    const { container, rerender } = render(<AliveTag value="dead" deadReason="nxdomain" />)
    expect(container.textContent).toBe('DNS不解析')
    rerender(<AliveTag value="dead" deadReason="no_http" />)
    expect(container.textContent).toBe('解析但HTTP不通')
  })
})
