import React, { useEffect, useState } from 'react'
import { App, Button, Form, Input, Modal, Popconfirm, Space, Switch, Table, Tag, Typography } from 'antd'
import { del, get, patch, post } from '../api'
import { HELP } from '../help'

// T24 客户托管规则：内置 + 自定义；可增、删、停用；改动后后台异步重算
export default function HostedRules() {
  const { message } = App.useApp()
  const [items, setItems] = useState([])
  const [loading, setLoading] = useState(false)
  const [open, setOpen] = useState(false)
  const [form] = Form.useForm()

  const load = async () => {
    setLoading(true)
    try {
      const d = await get('/api/customer-hosted-rules')
      setItems(d.items || [])
    } catch (e) {
      message.error(e.message)
    } finally {
      setLoading(false)
    }
  }
  useEffect(() => { load() }, [])

  const run = async (fn) => {
    try {
      await fn()
      message.success('已保存，后台重算资产标记中')
      load()
    } catch (e) {
      message.error(e.message)
    }
  }

  return (
    <Space direction="vertical" style={{ width: '100%' }} size="large">
      <Space style={{ width: '100%', justifyContent: 'space-between' }}>
        <Typography.Title level={3} style={{ margin: 0 }}>客户托管规则</Typography.Title>
        <Button type="primary" onClick={() => { form.resetFields(); setOpen(true) }}>添加规则</Button>
      </Space>
      <Typography.Text type="secondary">
        {HELP.hosted} 只匹配资产子域名本身（不看 CNAME）；<code>*.</code> 匹配一层或多层子域，标签内的 <code>*</code>（如 <code>oss-*</code>）只匹配本层。内置规则可停用或删除，删除后升级不会加回。
      </Typography.Text>
      <Table
        rowKey="id"
        loading={loading}
        dataSource={items}
        pagination={false}
        size="small"
        columns={[
          { title: '规则', dataIndex: 'pattern', render: (v) => <code>{v}</code> },
          { title: '来源', dataIndex: 'builtin', width: 80, render: (v) => (v ? <Tag color="blue">内置</Tag> : <Tag>自定义</Tag>) },
          { title: '说明', dataIndex: 'note', ellipsis: true },
          {
            title: '启用',
            dataIndex: 'enabled',
            width: 80,
            render: (v, r) => <Switch size="small" checked={v} onChange={(on) => run(() => patch(`/api/customer-hosted-rules/${r.id}`, { enabled: on }))} />,
          },
          {
            title: '操作',
            width: 90,
            render: (_, r) => (
              <Popconfirm title="删除该规则？" onConfirm={() => run(() => del(`/api/customer-hosted-rules/${r.id}`))}>
                <Button danger size="small">删除</Button>
              </Popconfirm>
            ),
          },
        ]}
      />
      <Modal
        title="添加客户托管规则"
        open={open}
        onCancel={() => setOpen(false)}
        onOk={async () => {
          const vals = await form.validateFields()
          await run(() => post('/api/customer-hosted-rules', vals))
          setOpen(false)
        }}
      >
        <Form form={form} layout="vertical">
          <Form.Item name="pattern" label="规则" rules={[{ required: true }, { pattern: /^\*\./, message: '须以 *. 开头' }]} extra="例：*.oss-*.aliyuncs.com">
            <Input placeholder="*.example-cloud.com" />
          </Form.Item>
          <Form.Item name="note" label="说明（云产品 / 文档）"><Input /></Form.Item>
        </Form>
      </Modal>
    </Space>
  )
}
