import React, { useEffect, useState } from 'react'
import {
  App, Button, Form, Modal, Select, Space, Switch, Table, Tag, Tooltip, Typography,
} from 'antd'
import dayjs from 'dayjs'
import { get, post } from '../api'
import { Guide, HELP, Hint } from '../help'

const statuses = ['pending', 'running', 'retrying', 'done', 'failed']
const kinds = ['passive', 'active', 'dns', 'http', 'expand']
const manual = new Set(['passive', 'active', 'expand'])

export default function Scans() {
  const { message } = App.useApp()
  const [vendors, setVendors] = useState([])
  const [roots, setRoots] = useState([])
  const [filters, setFilters] = useState({ degraded: false })
  const [items, setItems] = useState([])
  const [cursor, setCursor] = useState(null)
  const [loading, setLoading] = useState(false)
  const [open, setOpen] = useState(false)
  const [form] = Form.useForm()

  useEffect(() => {
    get('/api/vendors').then((d) => setVendors(d.items || [])).catch((e) => message.error(e.message))
  }, [])

  const loadRoots = async (vendor) => {
    if (!vendor) { setRoots([]); return }
    const d = await get(`/api/vendors/${vendor}/roots`)
    setRoots(d.items || [])
  }

  const load = async (next) => {
    setLoading(true)
    try {
      const params = new URLSearchParams({ limit: '50' })
      Object.entries(filters).forEach(([k, v]) => {
        if (v === undefined || v === null || v === '') return
        if (k === 'degraded' && v === false) return
        params.set(k, String(v))
      })
      if (next) params.set('cursor', next)
      const d = await get(`/api/scans?${params}`)
      setItems(next ? [...items, ...(d.items || [])] : (d.items || []))
      setCursor(d.next_cursor || null)
    } catch (e) {
      message.error(e.message)
    } finally {
      setLoading(false)
    }
  }
  useEffect(() => { load() }, [filters])

  const rerun = async (row) => {
    try {
      await post('/api/scans', { root_id: row.root_id, kind: row.kind })
      message.success(`已重跑 ${row.kind}`)
      load()
    } catch (e) {
      message.error(e.message)
    }
  }

  return (
    <Space direction="vertical" style={{ width: '100%' }} size="large">
      <Space style={{ width: '100%', justifyContent: 'space-between' }} wrap>
        <Hint text="扫描链路：passive / active 找子域 → dns 解析 → http 探测；expand 单独找新根域。"><Typography.Title level={3} style={{ margin: 0 }}>扫描任务</Typography.Title></Hint>
        <Button type="primary" onClick={() => { form.resetFields(); form.setFieldsValue({ kind: 'passive' }); setOpen(true) }}>触发扫描</Button>
      </Space>
      <Space wrap>
        <Select allowClear placeholder="厂商" style={{ width: 180 }} options={vendors.map((v) => ({ value: v.id, label: v.name }))}
          value={filters.vendor} onChange={(v) => { setFilters({ ...filters, vendor: v, root: undefined }); loadRoots(v) }} />
        <Select allowClear placeholder="根域" style={{ width: 200 }} options={roots.map((r) => ({ value: r.id, label: r.domain }))}
          value={filters.root} onChange={(v) => setFilters({ ...filters, root: v })} />
        <Select allowClear placeholder="状态" style={{ width: 140 }} options={statuses.map((s) => ({ value: s, label: s }))}
          value={filters.status} onChange={(v) => setFilters({ ...filters, status: v })} />
        <Select allowClear placeholder="类型" style={{ width: 140 }} options={kinds.map((k) => ({ value: k, label: <Tooltip title={HELP.kinds[k]} placement="right">{k}</Tooltip> }))}
          value={filters.kind} onChange={(v) => setFilters({ ...filters, kind: v })} />
        <span>仅降级 <Switch checked={!!filters.degraded} onChange={(v) => setFilters({ ...filters, degraded: v })} /></span>
      </Space>
      <Table
        rowKey="id"
        loading={loading}
        dataSource={items}
        pagination={false}
        locale={{ emptyText: <Guide title="还没有扫描任务" steps={['点「触发扫描」：选厂商、根域、类型', '新根域建议先跑 passive；要找新根域跑 expand']} /> }}
        columns={[
          { title: 'ID', dataIndex: 'id', width: 70 },
          { title: '类型', dataIndex: 'kind', width: 90, render: (k) => <Tooltip title={HELP.kinds[k]}><span style={{ borderBottom: '1px dashed' }}>{k}</span></Tooltip> },
          {
            title: '状态', dataIndex: 'status', width: 120,
            render: (s, r) => (
              <Space>
                <Tag color={s === 'done' ? (r.degraded ? 'orange' : 'green') : s === 'failed' ? 'red' : 'blue'}>{s}</Tag>
                {r.degraded && <Tag color="orange">degraded</Tag>}
              </Space>
            ),
          },
          { title: <Hint text={HELP.degraded}>降级原因</Hint>, dataIndex: 'degraded_reason', ellipsis: true, render: (v, r) => (r.degraded ? (v || '-') : '') },
          { title: '创建', dataIndex: 'created_at', width: 170, render: (v) => dayjs(v).format('YYYY-MM-DD HH:mm:ss') },
          {
            title: '操作', width: 160,
            render: (_, r) => (
              manual.has(r.kind)
                ? <Button size="small" onClick={() => rerun(r)}>重跑</Button>
                : <Tooltip title="dns/http 为链路步骤，请从 passive 重跑整条链"><Button size="small" disabled>从 passive 重跑</Button></Tooltip>
            ),
          },
        ]}
      />
      {cursor && <a onClick={() => load(cursor)}>加载更多</a>}
      <Modal
        title="触发扫描"
        open={open}
        onCancel={() => setOpen(false)}
        onOk={async () => {
          const vals = await form.validateFields()
          await post('/api/scans', { root_id: vals.root_id, kind: vals.kind })
          message.success('已触发')
          setOpen(false)
          load()
        }}
      >
        <Form form={form} layout="vertical" initialValues={{ kind: 'passive' }}>
          <Form.Item name="vendor" label="厂商" rules={[{ required: true }]}>
            <Select options={vendors.map((v) => ({ value: v.id, label: v.name }))} onChange={(v) => { form.setFieldValue('root_id', undefined); loadRoots(v) }} />
          </Form.Item>
          <Form.Item name="root_id" label="根域" rules={[{ required: true }]}>
            <Select options={roots.map((r) => ({ value: r.id, label: r.domain }))} />
          </Form.Item>
          <Form.Item name="kind" label="类型" rules={[{ required: true }]}>
            <Select options={[...manual].map((k) => ({ value: k, label: k }))} />
          </Form.Item>
          <Form.Item noStyle shouldUpdate>
            {() => <Typography.Paragraph type="secondary" style={{ marginTop: -8 }}>{HELP.kinds[form.getFieldValue('kind')]}</Typography.Paragraph>}
          </Form.Item>
        </Form>
      </Modal>
    </Space>
  )
}
