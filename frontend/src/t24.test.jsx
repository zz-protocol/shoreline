import { describe, expect, it } from 'vitest'
import { filterParams } from './eventsWindow'
import { HELP } from './help'

describe('T24 客户托管', () => {
  it('播报默认不带 include_customer_hosted，打开开关才带', () => {
    expect(filterParams({ level: 'asset' }).has('include_customer_hosted')).toBe(false)
    expect(filterParams({ level: 'asset', hosted: true }).get('include_customer_hosted')).toBe('true')
  })
  it('有中文说明', () => {
    expect(HELP.hosted).toContain('客户托管')
  })
})
