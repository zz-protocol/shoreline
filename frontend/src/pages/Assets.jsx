import React, { useEffect, useState } from 'react'
import { App, Input, Select, Space, Switch, Table, Typography } from 'antd'
import { get } from '../api'
import { Guide, HELP, Hint } from '../help'

export default function Assets() {
  const { message } = App.useApp()
  const [vendors, setVendors] = useState([])
  const [vendor, setVendor] = useState()
  const [q, setQ] = useState('')
  const [wild, setWild] = useState(false)
  const [items, setItems] = useState([])
  const [cursor, setCursor] = useState(null)
  const [loading, setLoading] = useState(false)

  useEffect(() => {
    get('/api/vendors').then((d) => setVendors(d.items || [])).catch((e) => message.error(e.message))
  }, [])

  const load = async (next) => {
    setLoading(true)
    try {
      const params = new URLSearchParams({ limit: '50', include_wildcard: String(wild) })
      if (vendor) params.set('vendor', vendor)
      if (q.trim()) params.set('q', q.trim())
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
  useEffect(() => { load() }, [vendor, wild])

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
        <Input.Search placeholder="子域 / 标题" value={q} onChange={(e) => setQ(e.target.value)} onSearch={() => load()} style={{ width: 240 }} />
        <span>
          <Hint text={HELP.wildcard}>含泛解析</Hint> <Switch checked={wild} onChange={setWild} />
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
          { title: <Hint text={HELP.scope}>范围内</Hint>, dataIndex: 'in_scope', render: (v) => (v ? '是' : '否') },
          { title: '泛解析', dataIndex: 'wildcard', render: (v) => (v ? '是' : '') },
          { title: '来源', dataIndex: 'sources', render: (v) => (v || []).join(', ') },
        ]}
      />
      {cursor && <a onClick={() => load(cursor)}>加载更多</a>}
    </Space>
  )
}
