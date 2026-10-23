import React, { useEffect, useState } from 'react'
import { App, Button, DatePicker, Input, Modal, Segmented, Select, Space, Switch, Table, Tag, Typography } from 'antd'
import { CopyOutlined } from '@ant-design/icons'
import dayjs from 'dayjs'
import { get } from '../api'
import { Guide, HELP, Hint } from '../help'
import { windowRange } from '../eventsWindow'
import { copyText } from '../clipboard'

export const ALIVE = {
  alive: { label: '存活', color: 'green' },
  dead: { label: '失活', color: 'red' },
  unknown: { label: '未知', color: 'default' },
}

export function AliveTag({ value, at }) {
  const a = ALIVE[value] || ALIVE.unknown
  return (
    <span title={at ? `最后存活 ${dayjs(at).format('YYYY-MM-DD HH:mm:ss')}` : undefined}>
      <Tag color={a.color}>{a.label}</Tag>
    </span>
  )
}

/** 与列表/整列复制共用的筛选参数（不含分页；T29）。 */
export function assetFilterParams({ vendor, q, wild, hosted, alive, aliveWin, aliveCustom }) {
  const p = new URLSearchParams({ include_wildcard: String(!!wild), include_customer_hosted: String(!!hosted) })
  if (vendor) p.set('vendor', String(vendor))
  if (q?.trim()) p.set('q', q.trim())
  if (alive) p.set('alive', alive)
  if (aliveWin && aliveWin !== 'custom') {
    const { since, until } = windowRange(aliveWin, null)
    if (since) p.set('alive_since', since)
    if (until) p.set('alive_until', until)
  } else if (aliveWin === 'custom' && aliveCustom?.[0] && aliveCustom?.[1]) {
    const start = typeof aliveCustom[0].toISOString === 'function' ? aliveCustom[0].toISOString() : aliveCustom[0]
    const end = typeof aliveCustom[1].toISOString === 'function' ? aliveCustom[1].toISOString() : aliveCustom[1]
    const { since, until } = windowRange('custom', [start, end])
    if (since) p.set('alive_since', since)
    if (until) p.set('alive_until', until)
  }
  return p
}

export default function Assets() {
  const { message } = App.useApp()
  const [vendors, setVendors] = useState([])
  const [vendor, setVendor] = useState()
  const [q, setQ] = useState('')
  const [wild, setWild] = useState(false)
  const [hosted, setHosted] = useState(false)
  const [alive, setAlive] = useState()
  const [aliveWin, setAliveWin] = useState('') // ''=不限；today/7d/30d/custom（T27）
  const [aliveCustom, setAliveCustom] = useState(null)
  const [items, setItems] = useState([])
  const [cursor, setCursor] = useState(null)
  const [loading, setLoading] = useState(false)
  const [manual, setManual] = useState(null)

  useEffect(() => {
    get('/api/vendors').then((d) => setVendors(d.items || [])).catch((e) => message.error(e.message))
  }, [])

  const filters = () => assetFilterParams({ vendor, q, wild, hosted, alive, aliveWin, aliveCustom })

  const load = async (next) => {
    setLoading(true)
    try {
      const params = filters()
      params.set('limit', '50')
      if (next) params.set('cursor', next)
      const d = await get(`/api/assets?${params}`)
      setItems(next ? [...items, ...(d.items || [])] : (d.items || []))
      setCursor(d.next_cursor || null)
    } catch (e) {
      message.error(e.message)
    } finally {
      setLoading(false)
    }
  }
  useEffect(() => {
    if (aliveWin === 'custom' && !(aliveCustom?.[0] && aliveCustom?.[1])) return
    load()
  }, [vendor, wild, hosted, alive, aliveWin, aliveCustom])

  const copyCol = async (field) => {
    try {
      const p = filters()
      p.set('field', field)
      const d = await get(`/api/assets/values?${p}`)
      const text = (d.items || []).join('\n')
      if (await copyText(text)) {
        message.success(`已复制 ${d.count} 条${d.truncated ? '（已截断至上限）' : ''}`)
      } else {
        setManual({ text, count: d.count, truncated: d.truncated })
      }
    } catch (e) {
      message.error(e.message)
    }
  }

  const colTitle = (title, field) => (
    <Space size={4}>
      {title}
      <Button size="small" type="text" icon={<CopyOutlined />} title="复制本列（当前筛选下全部结果）" onClick={() => copyCol(field)} data-testid={`copy-${field}`}>
        复制本列
      </Button>
    </Space>
  )

  return (
    <Space direction="vertical" style={{ width: '100%' }} size="large">
      <Typography.Title level={3} style={{ margin: 0 }}>资产</Typography.Title>
      <Space wrap>
        <Select
          allowClear
          placeholder="厂商"
          style={{ width: 200 }}
          options={vendors.map((v) => ({ value: v.id, label: v.name }))}
          value={vendor}
          onChange={setVendor}
        />
        <Hint text={HELP.alive}><Select
          allowClear
          placeholder="探活状态"
          style={{ width: 140 }}
          value={alive}
          onChange={setAlive}
          options={Object.entries(ALIVE).map(([value, a]) => ({ value, label: a.label }))}
        /></Hint>
        <Segmented
          value={aliveWin || 'all'}
          onChange={(v) => setAliveWin(v === 'all' ? '' : v)}
          options={[
            { value: 'all', label: '存活时间不限' },
            { value: 'today', label: '今日' },
            { value: '7d', label: '近 7 天' },
            { value: '30d', label: '近 30 天' },
            { value: 'custom', label: '自定义' },
          ]}
          data-testid="alive-win"
        />
        {aliveWin === 'custom' && (
          <DatePicker.RangePicker showTime value={aliveCustom} onChange={setAliveCustom} data-testid="alive-custom" />
        )}
        <Input.Search placeholder="子域 / 标题" value={q} onChange={(e) => setQ(e.target.value)} onSearch={() => load()} style={{ width: 240 }} />
        <span>
          <Hint text={HELP.wildcard}>含泛解析</Hint> <Switch checked={wild} onChange={setWild} />
        </span>
        <span>
          <Hint text={HELP.hosted}>含客户托管</Hint> <Switch checked={hosted} onChange={setHosted} data-testid="hosted-switch" />
        </span>
      </Space>
      <Typography.Text type="secondary">
        「复制本列」覆盖当前筛选条件下的全部结果（非仅本页），一行一个、去重；HTTP 下自动回退，失败可手动复制。
      </Typography.Text>
      <Table
        rowKey="id"
        loading={loading}
        dataSource={items}
        pagination={false}
        locale={{ emptyText: <Guide title="没有资产" steps={['先在「厂商 / 根域」添加根域', '再到「扫描任务」触发 passive，完成后子域会出现在这里']} /> }}
        columns={[
          { title: 'ID', dataIndex: 'id', width: 80 },
          { title: colTitle('子域', 'subdomain'), dataIndex: 'subdomain' },
          { title: colTitle('URL', 'url'), key: 'url', render: () => '' },
          { title: <Hint text={HELP.alive}>探活</Hint>, dataIndex: 'alive', width: 90, render: (v, r) => <AliveTag value={v} at={r.last_alive_at} /> },
          { title: '最后存活', dataIndex: 'last_alive_at', width: 170, render: (v) => (v ? dayjs(v).format('YYYY-MM-DD HH:mm:ss') : '') },
          { title: <Hint text={HELP.scope}>范围内</Hint>, dataIndex: 'in_scope', render: (v) => (v ? '是' : '否') },
          { title: '泛解析', dataIndex: 'wildcard', render: (v) => (v ? '是' : '') },
          { title: <Hint text={HELP.hosted}>客户托管</Hint>, dataIndex: 'customer_hosted', render: (v) => (v ? <Tag>客户托管</Tag> : '') },
          { title: '来源', dataIndex: 'sources', render: (v) => (v || []).join(', ') },
        ]}
      />
      {cursor && <a onClick={() => load(cursor)}>加载更多</a>}
      <Modal open={!!manual} title={`浏览器不允许自动复制，请手动复制（${manual?.count ?? 0} 条${manual?.truncated ? '，已截断至上限' : ''}）`} footer={null} onCancel={() => setManual(null)}>
        <Input.TextArea data-testid="manual-copy" value={manual?.text} rows={12} readOnly autoFocus onFocus={(e) => e.target.select()} />
      </Modal>
    </Space>
  )
}
