import React, { useEffect, useState } from 'react'
import { Alert, App, Button, Form, Input, Modal, Popconfirm, Space, Switch, Table, Typography } from 'antd'
import { del, get, patch, post } from '../api'
import { HELP, Hint } from '../help'

function parseInetAtonPart(p) {
  if (!p) return null
  if (/^0[xX][0-9a-fA-F]+$/.test(p)) {
    const n = parseInt(p.slice(2), 16)
    return Number.isFinite(n) ? n : null
  }
  if (p.length > 1 && p.startsWith('0')) {
    if (!/^[0-7]+$/.test(p)) return null
    const n = parseInt(p, 8)
    return Number.isFinite(n) ? n : null
  }
  if (!/^\d+$/.test(p)) return null
  const n = parseInt(p, 10)
  return Number.isFinite(n) ? n : null
}

function isInetAton(host) {
  const parts = host.split('.')
  if (parts.length < 1 || parts.length > 4) return false
  const nums = parts.map(parseInetAtonPart)
  if (nums.some((n) => n === null)) return false
  const [a, b, c, d] = nums
  if (parts.length === 1) return a <= 0xffffffff
  if (parts.length === 2) return a <= 0xff && b <= 0xffffff
  if (parts.length === 3) return a <= 0xff && b <= 0xff && c <= 0xffff
  return nums.every((n) => n <= 0xff)
}

export function validServiceScanPattern(p) {
  const v = (p || '').trim().toLowerCase()
  if (!v || v.length > 253) return false
  const host = v.startsWith('*.') ? v.slice(2) : v
  if (!host.includes('.') || host.includes('*') || host.includes(':')) return false
  const labels = host.split('.')
  if (!labels.every((l) => /^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$/.test(l))) return false
  if (/^\d+$/.test(labels[labels.length - 1])) return false
  if (isInetAton(host)) return false
  return true
}

// T49 扫描授权范围：全局开关默认关；仅对登记域及其子域允许非 Web 探测。
export default function ServiceScan() {
  const { message } = App.useApp()
  const [enabled, setEnabled] = useState(false)
  const [items, setItems] = useState([])
  const [loading, setLoading] = useState(false)
  const [open, setOpen] = useState(false)
  const [addErr, setAddErr] = useState('')
  const [form] = Form.useForm()

  const load = async () => {
    setLoading(true)
    try {
      const d = await get('/api/service-scan')
      setEnabled(!!d.enabled)
      setItems(d.items || [])
    } catch (e) {
      message.error(e.message)
    } finally {
      setLoading(false)
    }
  }
  useEffect(() => { load() }, [])

  const toggle = async (on) => {
    try {
      const d = await patch('/api/service-scan', { enabled: on })
      setEnabled(!!d.enabled)
      setItems(d.items || [])
      message.success(on ? '已开启非 Web 探测（仍须登记范围）' : '已关闭非 Web 探测')
    } catch (e) {
      message.error(e.message)
    }
  }

  return (
    <Space direction="vertical" style={{ width: '100%' }} size="large">
      <Space style={{ width: '100%', justifyContent: 'space-between' }}>
        <Hint text={HELP.serviceScan}><Typography.Title level={3} style={{ margin: 0 }}>扫描授权范围</Typography.Title></Hint>
        <Button type="primary" onClick={() => { form.resetFields(); setAddErr(''); setOpen(true) }}>添加范围</Button>
      </Space>
      <Typography.Text type="secondary">{HELP.serviceScan}</Typography.Text>
      <Space>
        <span>允许端口/服务探测</span>
        <Switch data-testid="service-scan-enabled" checked={enabled} onChange={toggle} />
      </Space>
      <Table
        rowKey="id"
        loading={loading}
        dataSource={items}
        pagination={false}
        size="small"
        data-testid="service-scan-scopes"
        columns={[
          { title: '域 / 模式', dataIndex: 'pattern', render: (v) => <code>{v}</code> },
          {
            title: '操作',
            width: 90,
            render: (_, r) => (
              <Popconfirm title="删除该范围？" onConfirm={async () => {
                try {
                  await del(`/api/service-scan/scopes/${r.id}`)
                  message.success('已删除')
                  load()
                } catch (e) {
                  message.error(e.message)
                }
              }}>
                <Button danger size="small" data-testid={`service-scan-del-${r.id}`}>删除</Button>
              </Popconfirm>
            ),
          },
        ]}
      />
      <Modal
        title="添加授权范围"
        open={open}
        onCancel={() => setOpen(false)}
        onOk={async () => {
          let vals
          try {
            vals = await form.validateFields()
          } catch {
            return
          }
          try {
            await post('/api/service-scan/scopes', vals)
          } catch (e) {
            setAddErr(e.message)
            return
          }
          setOpen(false)
          message.success('已添加')
          load()
        }}
      >
        {addErr && <Alert type="error" showIcon message={addErr} style={{ marginBottom: 12 }} data-testid="add-error" />}
        <Form form={form} layout="vertical" onValuesChange={() => setAddErr('')}>
          <Form.Item name="pattern" label="域" rules={[{ required: true, message: '请填写域' }, { validator: (_, v) => (!v || validServiceScanPattern(v) ? Promise.resolve() : Promise.reject(new Error('使用 example.test 或 *.example.test，不能是 IP'))) }]} extra="填写 example.test 将覆盖该域及其子域；*.example.test 只匹配子域。">
            <Input placeholder="example.test" data-testid="service-scan-pattern" />
          </Form.Item>
        </Form>
      </Modal>
    </Space>
  )
}
