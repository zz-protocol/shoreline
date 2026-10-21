import React, { useEffect, useState } from 'react'
import { App, Input, Select, Space, Switch, Table, Tag, Typography } from 'antd'
import dayjs from 'dayjs'
import { get } from '../api'
import { Guide, HELP, Hint } from '../help'

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

export default function Assets() {
  const { message } = App.useApp()
  const [vendors, setVendors] = useState([])
  const [vendor, setVendor] = useState()
  const [q, setQ] = useState('')
  const [wild, setWild] = useState(false)
  const [hosted, setHosted] = useState(false)
  const [alive, setAlive] = useState()
  const [items, setItems] = useState([])
  const [cursor, setCursor] = useState(null)
  const [loading, setLoading] = useState(false)

  useEffect(() => {
    get('/api/vendors').then((d) => setVendors(d.items || [])).catch((e) => message.error(e.message))
  }, [])

  const load = async (next) => {
    setLoading(true)
    try {
      const params = new URLSearchParams({ limit: '50', include_wildcard: String(wild), include_customer_hosted: String(hosted) })
      if (vendor) params.set('vendor', vendor)
      if (q.trim()) params.set('q', q.trim())
      if (alive) params.set('alive', alive)
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
  useEffect(() => { load() }, [vendor, wild, hosted, alive])

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
        <Input.Search placeholder="子域 / 标题" value={q} onChange={(e) => setQ(e.target.value)} onSearch={() => load()} style={{ width: 240 }} />
        <span>
          <Hint text={HELP.wildcard}>含泛解析</Hint> <Switch checked={wild} onChange={setWild} />
        </span>
        <span>
          <Hint text={HELP.hosted}>含客户托管</Hint> <Switch checked={hosted} onChange={setHosted} data-testid="hosted-switch" />
        </span>
      </Space>
      <Table
        rowKey="id"
        loading={loading}
        dataSource={items}
        pagination={false}
        locale={{ emptyText: <Guide title="没有资产" steps={['先在「厂商 / 根域」添加根域', '再到「扫描任务」触发 passive，完成后子域会出现在这里']} /> }}
        columns={[
          { title: 'ID', dataIndex: 'id', width: 80 },
          { title: '子域', dataIndex: 'subdomain' },
          { title: 'IP', dataIndex: 'ips', render: (v) => (v || []).join(', ') },
          { title: <Hint text={HELP.alive}>探活</Hint>, dataIndex: 'alive', width: 90, render: (v, r) => <AliveTag value={v} at={r.last_alive_at} /> },
          { title: '最后存活', dataIndex: 'last_alive_at', width: 170, render: (v) => (v ? dayjs(v).format('YYYY-MM-DD HH:mm:ss') : '') },
          { title: <Hint text={HELP.scope}>范围内</Hint>, dataIndex: 'in_scope', render: (v) => (v ? '是' : '否') },
          { title: '泛解析', dataIndex: 'wildcard', render: (v) => (v ? '是' : '') },
          { title: <Hint text={HELP.hosted}>客户托管</Hint>, dataIndex: 'customer_hosted', render: (v) => (v ? <Tag>客户托管</Tag> : '') },
          { title: '来源', dataIndex: 'sources', render: (v) => (v || []).join(', ') },
        ]}
      />
      {cursor && <a onClick={() => load(cursor)}>加载更多</a>}
    </Space>
  )
}
