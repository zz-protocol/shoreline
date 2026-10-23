import React, { useEffect, useState } from 'react'
import { App, Button, Form, Input, Modal, Popconfirm, Space, Table, Tag, Typography } from 'antd'
import { del, get, post, api } from '../api'

const LABELS = { fofa: 'FOFA', quake: 'Quake', hunter: 'Hunter' }
const SOURCE = { db: '网页', env: '环境变量', none: '未配置' }

export function maskPreview(k) {
  const v = (k || '').trim()
  if (!v) return ''
  if (v.length <= 4) return '****'
  return '****' + v.slice(-4)
}

export default function ProbeKeys() {
  const { message } = App.useApp()
  const [items, setItems] = useState([])
  const [loading, setLoading] = useState(false)
  const [edit, setEdit] = useState(null)
  const [form] = Form.useForm()

  const load = async () => {
    setLoading(true)
    try {
      const d = await get('/api/probe-keys')
      setItems(d.items || [])
    } catch (e) {
      message.error(e.message)
    } finally {
      setLoading(false)
    }
  }
  useEffect(() => { load() }, [])

  const runTest = async (provider) => {
    try {
      const d = await post(`/api/probe-keys/${provider}/test`)
      if (d.ok) message.success(`${LABELS[provider]} 连通正常（${SOURCE[d.source] || d.source}）`)
      else message.error(`${LABELS[provider]} 连通失败：${d.error || 'unknown'}`)
    } catch (e) {
      message.error(e.message)
    }
  }

  return (
    <Space direction="vertical" style={{ width: '100%' }} size="large">
      <Typography.Title level={3} style={{ margin: 0 }}>测绘 Key</Typography.Title>
      <Typography.Text type="secondary">
        配置 FOFA / Quake / Hunter 的查询 key，保存在数据库；页面与 API 只显示掩码（后 4 位），不返回明文。
        网页填写优先于环境变量；删除网页配置后回退到环境变量。改完后下次扫描生效，无需重启。
        「测试连通」只调用各服务的账户信息接口（只读，不消耗查询额度）。
      </Typography.Text>
      <Table
        rowKey="provider"
        loading={loading}
        dataSource={items}
        pagination={false}
        columns={[
          { title: '服务', dataIndex: 'provider', width: 120, render: (v) => LABELS[v] || v },
          {
            title: '来源',
            dataIndex: 'source',
            width: 120,
            render: (v) => {
              const color = v === 'db' ? 'blue' : v === 'env' ? 'default' : 'warning'
              return <Tag color={color}>{SOURCE[v] || v}</Tag>
            },
          },
          { title: '掩码', dataIndex: 'mask', render: (v) => (v ? <code>{v}</code> : '—') },
          {
            title: '操作',
            width: 280,
            render: (_, r) => (
              <Space>
                <Button size="small" onClick={() => { form.resetFields(); setEdit(r.provider) }}>
                  {r.source === 'db' ? '修改' : '设置'}
                </Button>
                <Button size="small" disabled={!r.configured} onClick={() => runTest(r.provider)} data-testid={`test-${r.provider}`}>
                  测试连通
                </Button>
                {r.source === 'db' && (
                  <Popconfirm
                    title="删除网页配置？将回退到环境变量"
                    onConfirm={async () => {
                      try {
                        await del(`/api/probe-keys/${r.provider}`)
                        message.success('已删除网页配置')
                        load()
                      } catch (e) {
                        message.error(e.message)
                      }
                    }}
                  >
                    <Button danger size="small">删除</Button>
                  </Popconfirm>
                )}
              </Space>
            ),
          },
        ]}
      />
      <Modal
        title={`设置 ${LABELS[edit] || ''} Key`}
        open={!!edit}
        onCancel={() => setEdit(null)}
        onOk={async () => {
          const vals = await form.validateFields()
          try {
            await api('PUT', `/api/probe-keys/${edit}`, { key: vals.key })
            message.success('已保存，下次扫描生效')
            setEdit(null)
            load()
          } catch (e) {
            message.error(e.message)
          }
        }}
      >
        <Form form={form} layout="vertical">
          <Form.Item name="key" label="API Key" rules={[{ required: true, message: '请输入 key' }]}>
            <Input.Password placeholder="明文只在提交时发送，列表不回显" autoComplete="off" />
          </Form.Item>
        </Form>
      </Modal>
    </Space>
  )
}
