import React, { useEffect, useRef, useState } from 'react'
import { Alert, App, Button, DatePicker, Input, Modal, Segmented, Select, Space, Switch, Table, Tag, Typography } from 'antd'
import { CopyOutlined } from '@ant-design/icons'
import dayjs from 'dayjs'
import { get } from '../api'
import { Guide, HELP, Hint } from '../help'
import { appendEvents, applyPoll, filterParams, maxId, statsLabel, TYPE_TAG, windowRange } from '../eventsWindow'
import { copyText } from '../clipboard'

const PAGE = 50

export default function Events() {
  const { message } = App.useApp()
  const [level, setLevel] = useState('asset')
  const [vendor, setVendor] = useState(null)
  const [hosted, setHosted] = useState(false)
  const [win, setWin] = useState('7d')
  const [custom, setCustom] = useState(null)
  const [vendors, setVendors] = useState([])
  const [items, setItems] = useState([])
  const [cursor, setCursor] = useState(null)
  const [stats, setStats] = useState(null)
  const [loading, setLoading] = useState(false)
  const [overflow, setOverflow] = useState(false)
  const [manual, setManual] = useState(null) // 复制失败时手动复制的文本
  const itemsRef = useRef([])
  const rangeRef = useRef(null)

  const params = () => filterParams({ level, vendor, hosted, ...rangeRef.current })
  const set = (list) => {
    itemsRef.current = list
    setItems(list)
  }

  const loadStats = async () => {
    try {
      setStats(await get(`/api/events/stats?${params()}`))
    } catch {
      /* 列表报错已提示 */
    }
  }

  // 筛选变化：按新时间窗重拉第一页（不再用 localStorage since 截断历史）
  const reload = async () => {
    rangeRef.current = windowRange(win, custom)
    setLoading(true)
    try {
      const p = params()
      p.set('limit', String(PAGE))
      const d = await get(`/api/events?${p}`)
      set(d.items || [])
      setOverflow(false)
      setCursor(d.next_cursor || null)
      loadStats()
    } catch (e) {
      message.error(e.message)
    } finally {
      setLoading(false)
    }
  }

  const more = async () => {
    setLoading(true)
    try {
      const p = params()
      p.set('limit', String(PAGE))
      p.set('cursor', cursor)
      const d = await get(`/api/events?${p}`)
      set(appendEvents(itemsRef.current, d.items))
      setCursor(d.next_cursor || null)
    } catch (e) {
      message.error(e.message)
    } finally {
      setLoading(false)
    }
  }

  // 轮询：after_id=当前最大 id，只把新事件放到顶部
  const poll = async () => {
    if (rangeRef.current?.until) return // 自定义窗口有截止时间，不轮询
    try {
      const p = params()
      p.set('limit', '500')
      p.set('after_id', String(maxId(itemsRef.current)))
      const d = await get(`/api/events?${p}`)
      const r = applyPoll(itemsRef.current, d)
      if (r.overflow) {
        setOverflow(true)
      } else if (d.items?.length) {
        set(r.items)
        loadStats()
      }
    } catch {
      /* 静默 */
    }
  }

  const copyCol = async (field) => {
    try {
      const p = params()
      p.set('field', field)
      const d = await get(`/api/events/values?${p}`)
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

  useEffect(() => {
    get('/api/vendors').then((d) => setVendors(d.items || [])).catch(() => {})
  }, [])

  useEffect(() => {
    if (win === 'custom' && !(custom?.[0] && custom?.[1])) return
    reload()
    const t = setInterval(poll, 30000)
    return () => clearInterval(t)
  }, [level, vendor, hosted, win, custom])

  const name = (id) => vendors.find((v) => v.id === id)?.name || `#${id}`
  const colTitle = (title, field) => (
    <Space size={4}>
      {title}
      <Button size="small" type="text" icon={<CopyOutlined />} title="复制本列（所选时间窗内全部记录）" onClick={() => copyCol(field)}>
        复制本列
      </Button>
    </Space>
  )

  return (
    <Space direction="vertical" style={{ width: '100%' }} size="large">
      <Space style={{ width: '100%', justifyContent: 'space-between' }} wrap>
        <Typography.Title level={3} style={{ margin: 0 }}>新增播报</Typography.Title>
        <Space wrap>
          <Segmented
            value={win}
            onChange={setWin}
            options={[
              { value: 'today', label: '今日' },
              { value: '7d', label: '近 7 天' },
              { value: '30d', label: '近 30 天' },
              { value: 'custom', label: '自定义' },
            ]}
          />
          {win === 'custom' && <DatePicker.RangePicker showTime value={custom} onChange={setCustom} />}
          <Select
            allowClear
            placeholder="全部厂商"
            value={vendor}
            style={{ width: 180 }}
            options={vendors.map((v) => ({ value: v.id, label: v.name }))}
            onChange={(v) => setVendor(v ?? null)}
          />
          <Hint text={HELP.level}><Select
            value={level}
            style={{ width: 120 }}
            options={[
              { value: 'asset', label: '资产级' },
              { value: 'url', label: 'URL 级' },
              { value: 'all', label: '全部' },
            ]}
            onChange={setLevel}
          /></Hint>
          <span>
            <Hint text={HELP.hosted}>含客户托管</Hint> <Switch checked={hosted} onChange={setHosted} />
          </span>
        </Space>
      </Space>
      {stats && (
        <Space wrap data-testid="stats">
          <Typography.Text>时间窗内 {statsLabel(stats.totals)}</Typography.Text>
          {(stats.items || []).map((s) => (
            <Tag key={s.vendor_id} color="blue" style={{ cursor: 'pointer' }} onClick={() => setVendor(s.vendor_id)}>
              {name(s.vendor_id)}：{statsLabel(s)}
            </Tag>
          ))}
        </Space>
      )}
      {overflow && (
        <Alert
          type="info"
          showIcon
          style={{ cursor: 'pointer' }}
          message="有更多新事件，点此刷新"
          onClick={reload}
          data-testid="overflow"
        />
      )}
      <Typography.Text type="secondary">
        默认近 7 天，事件永久保留；每 30 秒只把新事件加到顶部；统计与「复制本列」覆盖所选时间窗内全部记录（非仅当前已加载）。
      </Typography.Text>
      <Table
        rowKey="id"
        loading={loading}
        dataSource={items}
        pagination={false}
        locale={{ emptyText: <Guide title="所选时间窗内没有新增" steps={['扫描发现的新子域 / 新站点会出现在这里，每 30 秒自动刷新', '还没扫描？到「扫描任务」触发 passive']} /> }}
        columns={[
          { title: '时间', dataIndex: 'created_at', render: (v) => dayjs(v).format('YYYY-MM-DD HH:mm:ss'), width: 180 },
          { title: '厂商', dataIndex: 'vendor_id', render: name, width: 120 },
          { title: '类型', dataIndex: 'type', render: (v) => <Tag color={TYPE_TAG[v]?.color}>{TYPE_TAG[v]?.label || v}</Tag>, width: 90 },
          { title: '层级', dataIndex: 'level', width: 80 },
          { title: colTitle('子域', 'subdomain'), dataIndex: 'subdomain' },
          { title: colTitle('URL', 'url'), dataIndex: 'url' },
          {
            title: '嵌套 URL 事件',
            dataIndex: 'url_events',
            render: (list) => (list?.length ? `${list.length} 条` : ''),
          },
        ]}
      />
      {cursor && <Button block onClick={more} loading={loading}>加载更多</Button>}
      <Modal open={!!manual} title={`浏览器不允许自动复制，请手动复制（${manual?.count ?? 0} 条${manual?.truncated ? '，已截断至上限' : ''}）`} footer={null} onCancel={() => setManual(null)}>
        <Input.TextArea data-testid="manual-copy" value={manual?.text} rows={12} readOnly autoFocus onFocus={(e) => e.target.select()} />
      </Modal>
    </Space>
  )
}
