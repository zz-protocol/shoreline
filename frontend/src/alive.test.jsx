import React from 'react'
import { describe, expect, it } from 'vitest'
import { render } from '@testing-library/react'
import { ALIVE, ALIVE_FILTER, AliveTag, assetFilterParams } from './pages/Assets'
import { HELP } from './help'

describe('探活展示', () => {
  it('三种状态中文标签', () => {
    expect(Object.keys(ALIVE)).toEqual(['alive', 'dead', 'unknown'])
    const { container, rerender } = render(<AliveTag value="alive" at="2026-10-21T01:00:00Z" />)
    expect(container.textContent).toBe('存活')
    expect(container.querySelector('span').getAttribute('title')).toMatch(/^最后存活 2026-10-21/)
    rerender(<AliveTag value="dead" />)
    expect(container.textContent).toBe('失活')
    rerender(<AliveTag value="dead" deadReason="nxdomain" />)
    expect(container.textContent).toBe('DNS不解析')
    rerender(<AliveTag value="dead" deadReason="no_http" />)
    expect(container.textContent).toBe('解析但HTTP不通')
    rerender(<AliveTag value={undefined} />)
    expect(container.textContent).toBe('未知')
  })
  it('筛选拆成两类死亡并带 dead_reason', () => {
    expect(ALIVE_FILTER.map((o) => o.value)).toEqual(['alive', 'nxdomain', 'no_http', 'unknown'])
    const nx = assetFilterParams({ alive: 'dead', deadReason: 'nxdomain' })
    expect(nx.get('alive')).toBe('dead')
    expect(nx.get('dead_reason')).toBe('nxdomain')
    const http = assetFilterParams({ alive: 'dead', deadReason: 'no_http' })
    expect(http.get('dead_reason')).toBe('no_http')
  })
  it('说明文案含复查规则', () => {
    expect(HELP.alive).toMatch(/不再复查/)
    expect(HELP.alive).toMatch(/SCHEDULE_ALIVE/)
    expect(HELP.alive).toMatch(/DNS不解析/)
    expect(HELP.alive).toMatch(/解析但HTTP不通/)
  })
})
