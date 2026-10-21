import React, { useEffect, useState } from 'react'
import {
  App, Button, Form, Input, Modal, Space, Switch, Table, Tabs, Typography, Popconfirm,
} from 'antd'
import { del, get, patch, post } from '../api'

export default function Vendors() {
  const { message } = App.useApp()
  const [vendors, setVendors] = useState([])
  const [cur, setCur] = useState(null)
  const [roots, setRoots] = useState([])
  const [scopes, setScopes] = useState([])
  const [cands, setCands] = useState([])
  const [loading, setLoading] = useState(false)
  const [vOpen, setVOpen] = useState(false)
  const [rOpen, setROpen] = useState(false)
  const [vForm] = Form.useForm()
  const [rForm] = Form.useForm()

  const loadVendors = async () => {
    setLoading(true)
    try {
      const d = await get('/api/vendors')
      setVendors(d.items || [])
    } catch (e) {
      message.error(e.message)
    } finally {
      setLoading(false)
    }
  }
  const loadDetail = async (v) => {
    setCur(v)
    try {
      const [r, s, c] = await Promise.all([
        get(`/api/vendors/${v.id}/roots`),
        get(`/api/vendors/${v.id}/scopes`),
        get(`/api/vendors/${v.id}/root-candidates?status=pending`),
      ])
      setRoots(r.items || [])
      setScopes(s.items || [])
      setCands(c.items || [])
    } catch (e) {
      message.error(e.message)
    }
  }
  useEffect(() => { loadVendors() }, [])

  return (
    <Space direction="vertical" style={{ width: '100%' }} size="large">
      <Space style={{ width: '100%', justifyContent: 'space-between' }}>
        <Typography.Title level={3} style={{ margin: 0 }}>厂商 / 根域</Typography.Title>
        <Button type="primary" onClick={() => { vForm.resetFields(); setVOpen(true) }}>新建厂商</Button>
      </Space>
      <Table
        rowKey="id"
        loading={loading}
        dataSource={vendors}
        pagination={false}
        onRow={(r) => ({ onClick: () => loadDetail(r), style: { cursor: 'pointer' } })}
        columns={[
          { title: 'ID', dataIndex: 'id', width: 80 },
          { title: '名称', dataIndex: 'name' },
          { title: '主体名', dataIndex: 'company_name' },
          {
            title: '主动发现',
            dataIndex: 'active_enabled',
            render: (v, r) => (
              <Switch
                checked={!!v}
                onClick={(_, e) => e.stopPropagation()}
                onChange={async (checked) => {
                  try {
                    await patch(`/api/vendors/${r.id}`, { active_enabled: checked })
                    message.success('已更新')
                    loadVendors()
                    if (cur?.id === r.id) loadDetail({ ...r, active_enabled: checked })
                  } catch (e) {
                    message.error(e.message)
                  }
                }}
              />
            ),
          },
          {
            title: '操作',
            render: (_, r) => (
              <Popconfirm
                title="确认删除该厂商？将级联删除下属全部数据"
                onConfirm={async () => {
                  try {
                    await del(`/api/vendors/${r.id}`)
                    message.success('已删除')
                    if (cur?.id === r.id) setCur(null)
                    loadVendors()
                  } catch (err) {
                    message.error(err.message)
                  }
                }}
              >
                <Button danger size="small" onClick={(e) => e.stopPropagation()}>删除</Button>
              </Popconfirm>
            ),
          },
        ]}
      />
      {cur && (
        <Tabs
          items={[
            {
              key: 'roots',
              label: `根域（${roots.length}）`,
              children: (
                <>
                  <Button type="dashed" onClick={() => { rForm.resetFields(); setROpen(true) }} style={{ marginBottom: 12 }}>添加根域</Button>
                  <Table
                    rowKey="id"
                    dataSource={roots}
                    pagination={false}
                    columns={[
                      { title: 'ID', dataIndex: 'id', width: 80 },
                      { title: '域名', dataIndex: 'domain' },
                      {
                        title: '操作',
                        render: (_, r) => (
                          <Popconfirm
                            title="确认删除该根域？将级联删除下属资产/事件/扫描"
                            onConfirm={async () => {
                              try {
                                await del(`/api/vendors/${cur.id}/roots/${r.id}`)
                                message.success('已删除')
                                loadDetail(cur)
                              } catch (err) {
                                message.error(err.message)
                              }
                            }}
                          >
                            <Button danger size="small">删除</Button>
                          </Popconfirm>
                        ),
                      },
                    ]}
                  />
                </>
              ),
            },
            {
              key: 'scopes',
              label: `范围（${scopes.length}）`,
              children: (
                <Table
                  rowKey="id"
                  dataSource={scopes}
                  pagination={false}
                  columns={[
                    { title: '类型', dataIndex: 'kind' },
                    { title: '规则', dataIndex: 'pattern' },
                    {
                      title: '操作',
                      render: (_, r) => (
                        <Popconfirm
                          title="删除规则并重算 in_scope？"
                          onConfirm={async () => {
                            try {
                              await del(`/api/vendors/${cur.id}/scopes/${r.id}`)
                              message.success('已删除')
                              loadDetail(cur)
                            } catch (err) {
                              message.error(err.message)
                            }
                          }}
                        >
                          <Button danger size="small">删除</Button>
                        </Popconfirm>
                      ),
                    },
                  ]}
                />
              ),
            },
            {
              key: 'cands',
              label: `待确认根域（${cands.length}）`,
              children: (
                <Table
                  rowKey="id"
                  dataSource={cands}
                  pagination={false}
                  columns={[
                    { title: '域名', dataIndex: 'domain' },
                    { title: '来源', dataIndex: 'sources', render: (s) => (s || []).join(', ') },
                    {
                      title: '操作',
                      render: (_, r) => (
                        <Space>
                          <Button size="small" type="primary" onClick={async () => {
                            try {
                              await post(`/api/root-candidates/${r.id}/confirm`, {})
                              message.success('已确认')
                              loadDetail(cur)
                            } catch (e) { message.error(e.message) }
                          }}>确认</Button>
                          <Button size="small" onClick={async () => {
                            try {
                              await post(`/api/root-candidates/${r.id}/ignore`, {})
                              message.success('已忽略')
                              loadDetail(cur)
                            } catch (e) { message.error(e.message) }
                          }}>忽略</Button>
                        </Space>
                      ),
                    },
                  ]}
                />
              ),
            },
          ]}
        />
      )}
      <Modal
        title="新建厂商"
        open={vOpen}
        onCancel={() => setVOpen(false)}
        onOk={async () => {
          const vals = await vForm.validateFields()
          await post('/api/vendors', vals)
          message.success('已创建')
          setVOpen(false)
          loadVendors()
        }}
      >
        <Form form={vForm} layout="vertical">
          <Form.Item name="name" label="名称" rules={[{ required: true }]}><Input /></Form.Item>
          <Form.Item name="company_name" label="主体名"><Input /></Form.Item>
        </Form>
      </Modal>
      <Modal
        title="添加根域"
        open={rOpen}
        onCancel={() => setROpen(false)}
        onOk={async () => {
          const vals = await rForm.validateFields()
          await post(`/api/vendors/${cur.id}/roots`, vals)
          message.success('已添加')
          setROpen(false)
          loadDetail(cur)
        }}
      >
        <Form form={rForm} layout="vertical">
          <Form.Item name="domain" label="域名" rules={[{ required: true }]}><Input placeholder="example.com" /></Form.Item>
        </Form>
      </Modal>
    </Space>
  )
}
