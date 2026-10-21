import React from 'react'
import { describe, expect, it } from 'vitest'
import { render } from '@testing-library/react'
import { ALIVE, AliveTag } from './pages/Assets'
import { HELP } from './help'

describe('探活展示', () => {
  it('三种状态中文标签', () => {
    expect(Object.keys(ALIVE)).toEqual(['alive', 'dead', 'unknown'])
    const { container, rerender } = render(<AliveTag value="alive" at="2026-10-21T01:00:00Z" />)
    expect(container.textContent).toBe('存活')
    expect(container.querySelector('span').getAttribute('title')).toMatch(/^最后存活 2026-10-21/)
    rerender(<AliveTag value="dead" />)
    expect(container.textContent).toBe('失活')
    rerender(<AliveTag value={undefined} />)
    expect(container.textContent).toBe('未知')
  })
  it('说明文案含复查规则', () => {
    expect(HELP.alive).toMatch(/不再复查/)
    expect(HELP.alive).toMatch(/SCHEDULE_ALIVE/)
  })
})
