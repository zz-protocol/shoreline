import { describe, expect, it } from 'vitest'
import { assetFilterParams } from './pages/Assets'

describe('T29 assetFilterParams', () => {
  it('列表与复制共用筛选；空 q / 不限存活时间不传对应参数', () => {
    const p = assetFilterParams({ vendor: 3, q: '  Api. ', wild: true, hosted: false, alive: 'alive', aliveWin: '' })
    expect(p.get('vendor')).toBe('3')
    expect(p.get('q')).toBe('Api.')
    expect(p.get('include_wildcard')).toBe('true')
    expect(p.get('include_customer_hosted')).toBe('false')
    expect(p.get('alive')).toBe('alive')
    expect(p.has('alive_since')).toBe(false)
    expect(assetFilterParams({ aliveWin: '' }).has('q')).toBe(false)
  })
  it('近 7 天带 alive_since', () => {
    const p = assetFilterParams({ aliveWin: '7d' })
    expect(p.has('alive_since')).toBe(true)
  })
})
