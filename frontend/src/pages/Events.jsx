import React, { useEffect, useRef, useState } from 'react'
import { App, Select, Space, Table, Tag, Typography } from 'antd'
import dayjs from 'dayjs'
import { get } from '../api'
import { advanceSince, initialSince, mergeEvents, resetSince } from '../eventsSince'

export default function Events() {
  const { message } = App.useApp()
  const [level, setLevel] = useState('asset')
  const [items, setItems] = useState([])
  const [loading, setLoading] = useState(false)
  const sinceRef = useRef(null)
  const itemsRef = useRef([])

  const load = async (quiet) => {
    if (!quiet) setLoading(true)
    try {
      if (!sinceRef.current) sinceRef.current = initialSince()
      const params = new URLSearchParams({ limit: '50', level, type: 'new' })
      params.set('since', sinceRef.current)
      const d = await get(`/api/events?${params}`)
      const list = d.items || []
      sinceRef.current = advanceSince(sinceRef.current, list)
      const merged = quiet ? mergeEvents(itemsRef.current, list) : mergeEvents([], list)
      itemsRef.current = merged
      setItems(merged)
    } catch (e) {
      if (!quiet) message.error(e.message)
    } finally {
      if (!quiet) setLoading(false)
    }
  }

  useEffect(() => {
    sinceRef.current = resetSince() // 切换 level：清 localStorage since，重拉近一天
    itemsRef.current = []
    setItems([])
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
      <Typography.Text type="secondary">
        每 30 秒轮询；since 首次为现在−1天（或 localStorage），之后按本轮最新事件时间推进；切换 level 会清 since 并重拉近一天；本地按 id 去重兜底。
      </Typography.Text>
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
