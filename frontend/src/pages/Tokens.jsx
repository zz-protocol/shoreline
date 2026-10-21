import React, { useEffect, useState } from 'react'
import { App, Button, Form, Input, Modal, Popconfirm, Space, Table, Typography } from 'antd'
import dayjs from 'dayjs'
import { del, get, post } from '../api'

export default function Tokens() {
  const { message } = App.useApp()
  const [items, setItems] = useState([])
  const [loading, setLoading] = useState(false)
  const [open, setOpen] = useState(false)
  const [form] = Form.useForm()

  const load = async () => {
    setLoading(true)
    try {
      const d = await get('/api/tokens')
      setItems(d.items || [])
    } catch (e) {
      message.error(e.message)
    } finally {
      setLoading(false)
    }
  }
  useEffect(() => { load() }, [])

  return (
    <Space direction="vertical" style={{ width: '100%' }} size="large">
      <Space style={{ width: '100%', justifyContent: 'space-between' }}>
        <Typography.Title level={3} style={{ margin: 0 }}>API Key</Typography.Title>
        <Button type="primary" onClick={() => { form.resetFields(); setOpen(true) }}>创建</Button>
      </Space>
      <Table
        rowKey="id"
        loading={loading}
        dataSource={items}
        pagination={false}
        columns={[
          { title: 'ID', dataIndex: 'id', width: 80 },
          { title: '名称', dataIndex: 'name' },
          { title: '创建', dataIndex: 'created_at', render: (v) => dayjs(v).format('YYYY-MM-DD HH:mm:ss') },
          { title: '最近使用', dataIndex: 'last_used_at', render: (v) => (v ? dayjs(v).format('YYYY-MM-DD HH:mm:ss') : '-') },
          {
            title: '操作',
            render: (_, r) => (
              <Popconfirm
                title="吊销该 token？"
                onConfirm={async () => {
                  try {
                    await del(`/api/tokens/${r.id}`)
                    message.success('已吊销')
                    load()
                  } catch (e) {
                    message.error(e.message)
                  }
                }}
              >
                <Button danger size="small">删除</Button>
              </Popconfirm>
            ),
          },
        ]}
      />
      <Modal
        title="创建 API Key"
        open={open}
        onCancel={() => setOpen(false)}
        onOk={async () => {
          const vals = await form.validateFields()
          const d = await post('/api/tokens', vals)
          setOpen(false)
          Modal.info({
            title: '请立即复制 token（只显示一次）',
            width: 560,
            content: <Typography.Paragraph copyable code>{d.token}</Typography.Paragraph>,
          })
          load()
        }}
      >
        <Form form={form} layout="vertical">
          <Form.Item name="name" label="名称" rules={[{ required: true }]}><Input /></Form.Item>
        </Form>
      </Modal>
    </Space>
  )
}
