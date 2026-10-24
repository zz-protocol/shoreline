import React, { useEffect, useState } from 'react'
import { Alert, App, Button, Form, Input, Modal, Popconfirm, Space, Switch, Table, Tag, Typography } from 'antd'
import { del, get, patch, post } from '../api'
import { HELP } from '../help'

/** 前端只做格式初筛；是否为公共后缀由后端 publicsuffix 判断，400 时在弹窗内显示。
 *  两种写法（T31）：`*.` 开头（匹配 ≥1 层）；或不以 `*.` 开头但至少一个标签含 *（标签数须一致、逐层匹配，不能出现 **）。 */
export function validHostedPattern(p) {
  const v = (p || '').trim().toLowerCase()
  if (!v || v.length > 253) return false
  const wildcardPrefix = v.startsWith('*.')
  if (!wildcardPrefix && (!v.includes('*') || v.includes('**'))) return false
  return (wildcardPrefix ? v.slice(2) : v).split('.').every((l) => /^[a-z0-9*-]+$/.test(l))
}

// T24 客户托管规则：内置 + 自定义；可增、删、停用；改动后后台异步重算
export default function HostedRules() {
  const { message } = App.useApp()
  const [items, setItems] = useState([])
  const [loading, setLoading] = useState(false)
  const [open, setOpen] = useState(false)
  const [addErr, setAddErr] = useState('')
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
        <Button type="primary" onClick={() => { form.resetFields(); setAddErr(''); setOpen(true) }}>添加规则</Button>
      </Space>
      <Typography.Text type="secondary">
        {HELP.hosted} 只匹配资产子域名本身（不看 CNAME）。两种写法：① <code>*.</code> 开头匹配一层或多层子域；② 不以 <code>*.</code> 开头、但标签里含 <code>*</code> 的规则要求层数与主机名完全一致、逐层匹配（如 <code>*-vitesandbox.miaoda.cn</code> 命中 <code>app-x-vitesandbox.miaoda.cn</code>，不命中 <code>api.miaoda.cn</code>、<code>a.b-vitesandbox.miaoda.cn</code>）。标签内的 <code>*</code>（如 <code>oss-*</code>）只匹配本层。内置规则可停用或删除，删除后升级不会加回。
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
          let vals
          try {
            vals = await form.validateFields()
          } catch {
            return // 表单校验失败：保持打开，字段下已显示错误
          }
          try {
            await post('/api/customer-hosted-rules', vals)
          } catch (e) {
            setAddErr(e.message) // 400/409：保持打开、保留输入
            return
          }
          setOpen(false)
          message.success('已保存，后台重算资产标记中')
          load()
        }}
      >
        {addErr && <Alert type="error" showIcon message={addErr} style={{ marginBottom: 12 }} data-testid="add-error" />}
        <Form form={form} layout="vertical" onValuesChange={() => setAddErr('')}>
          <Form.Item name="pattern" label="规则" rules={[{ required: true, message: '请填写规则' }, { validator: (_, v) => (!v || validHostedPattern(v) ? Promise.resolve() : Promise.reject(new Error('须以 *. 开头（匹配 ≥1 层），或在某个标签里含 *（精确层数，如 *-sandbox.example.com）；标签只能含 a-z0-9-*'))) }]} extra="两种写法：*.oss-*.aliyuncs.com（匹配 ≥1 层）；*-vitesandbox.miaoda.cn（不以 *. 开头：层数必须一致，逐层匹配）">
            <Input placeholder="*.example-cloud.com" />
          </Form.Item>
          <Form.Item name="note" label="说明（云产品 / 文档）"><Input /></Form.Item>
        </Form>
      </Modal>
    </Space>
  )
}
