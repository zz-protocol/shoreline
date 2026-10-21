import React, { useEffect, useRef, useState } from 'react'
import { App, Select, Space, Table, Tag, Typography } from 'antd'
import dayjs from 'dayjs'
import { get } from '../api'

export default function Events() {
  const { message } = App.useApp()
  const [level, setLevel] = useState('asset')
  const [items, setItems] = useState([])
  const [loading, setLoading] = useState(false)
  const sinceRef = useRef(null)

  const load = async (quiet) => {
    if (!quiet) setLoading(true)
    try {
      const params = new URLSearchParams({ limit: '50', level, type: 'new' })
      if (sinceRef.current) params.set('since', sinceRef.current)
      const d = await get(`/api/events?${params}`)
      const list = d.items || []
      setItems(list)
      if (list.length && !sinceRef.current) {
        // 首屏：下次轮询从当前最新往前一点点，避免漏
        sinceRef.current = dayjs().subtract(1, 'day').toISOString()
      }
    } catch (e) {
      if (!quiet) message.error(e.message)
    } finally {
      if (!quiet) setLoading(false)
    }
  }

  useEffect(() => {
    sinceRef.current = dayjs().subtract(1, 'day').toISOString()
    load()
    const t = setInterval(() => load(true), 30000)
    return () => clearInterval(t)
  }, [level])

  return (
    <Space direction="vertical" style={{ width: '100%' }} size="large">
      <Space style={{ width: '100%', justifyContent: 'space-between' }}>
        <Typography.Title level={3} style={{ margin: 0 }}>新增播报</Typography.Title>
        <Select
          value={level}
          style={{ width: 160 }}
          options={[
            { value: 'asset', label: '资产级' },
            { value: 'url', label: 'URL 级' },
            { value: 'all', label: '全部' },
          ]}
          onChange={setLevel}
        />
      </Space>
      <Typography.Text type="secondary">每 30 秒轮询 GET /api/events?since=（不做 SSE）</Typography.Text>
      <Table
        rowKey="id"
        loading={loading}
        dataSource={items}
        pagination={false}
        columns={[
          { title: '时间', dataIndex: 'created_at', render: (v) => dayjs(v).format('YYYY-MM-DD HH:mm:ss'), width: 180 },
          { title: '类型', dataIndex: 'type', render: (v) => <Tag color="green">{v}</Tag>, width: 90 },
          { title: '层级', dataIndex: 'level', width: 80 },
          { title: '子域', dataIndex: 'subdomain' },
          { title: 'URL', dataIndex: 'url' },
          {
            title: '嵌套 URL 事件',
            dataIndex: 'url_events',
            render: (list) => (list?.length ? `${list.length} 条` : ''),
          },
        ]}
      />
    </Space>
  )
}
