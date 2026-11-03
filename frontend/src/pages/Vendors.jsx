import React, { useEffect, useRef, useState } from 'react'
import {
  App, Button, Form, Input, Modal, Select, Space, Switch, Table, Tabs, Typography, Popconfirm,
} from 'antd'
import { del, get, patch, post } from '../api'
import { Guide, HELP, Hint } from '../help'
import ExpandPanel from './ExpandPanel'

export default function Vendors() {
  const { message } = App.useApp()
  const [vendors, setVendors] = useState([])
  const [cur, setCur] = useState(null)
  const [roots, setRoots] = useState(null)
  const [scopes, setScopes] = useState([])
  const [cands, setCands] = useState([])
  const [loading, setLoading] = useState(false)
  const [vOpen, setVOpen] = useState(false)
  const [addFor, setAddFor] = useState(null) // T34：正在行内添加根域的厂商 id
  const [vForm] = Form.useForm()
  const [sForm] = Form.useForm()

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
  const detailSeq = useRef(0)
  // 切换厂商先清空明细（roots=null 表示加载中，expand 按钮禁用）；迟到的旧厂商响应丢弃
  const loadDetail = async (v) => {
    const seq = ++detailSeq.current
    if (cur?.id !== v.id) {
      setRoots(null)
      setScopes([])
      setCands([])
    }
    setCur(v)
    try {
      const [r, s, c] = await Promise.all([
        get(`/api/vendors/${v.id}/roots`),
        get(`/api/vendors/${v.id}/scopes`),
        get(`/api/vendors/${v.id}/root-candidates?status=pending`),
      ])
      if (seq !== detailSeq.current) return
      setRoots(r.items || [])
      setScopes(s.items || [])
      setCands(c.items || [])
    } catch (e) {
      if (seq === detailSeq.current) message.error(e.message)
    }
  }
  useEffect(() => { loadVendors() }, [])

  return (
    <Space direction="vertical" style={{ width: '100%' }} size="large">
      <Space style={{ width: '100%', justifyContent: 'space-between' }}>
        <Hint text={HELP.vendor}><Typography.Title level={3} style={{ margin: 0 }}>厂商 / 根域</Typography.Title></Hint>
        <Button type="primary" onClick={() => { vForm.resetFields(); setVOpen(true) }}>新建厂商</Button>
      </Space>
      <Table
        rowKey="id"
        loading={loading}
        dataSource={vendors}
        pagination={false}
        locale={{ emptyText: <Guide title="还没有厂商" steps={['1. 点右上「新建厂商」，填名称（主体名可选，用于 ICP 扩展）', '2. 在厂商行点「添加根域」，就地填写', '3. 到「扫描任务」触发 passive 扫描；点厂商行后可「一键发现新根域」']} /> }}
        expandable={{
          showExpandColumn: false,
          expandedRowKeys: addFor != null ? [addFor] : [],
          expandedRowRender: (v) => (
            <InlineRootAdd vendor={v} roots={cur?.id === v.id ? roots : null}
              onAdded={() => loadDetail(v)} onClose={() => setAddFor(null)} />
          ),
        }}
        onRow={(r) => ({ onClick: () => loadDetail(r), style: { cursor: 'pointer' } })}
        columns={[
          { title: 'ID', dataIndex: 'id', width: 80 },
          { title: '名称', dataIndex: 'name' },
          { title: '主体名', dataIndex: 'company_name' },
          {
            title: <Hint text={HELP.activeSwitch}>主动发现</Hint>,
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
              <Space>
              <Button size="small" data-testid={`add-root-${r.id}`}
                onClick={(e) => { e.stopPropagation(); setAddFor(addFor === r.id ? null : r.id); if (addFor !== r.id) loadDetail(r) }}>
                添加根域
              </Button>
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
              </Space>
            ),
          },
        ]}
      />
      {!cur && vendors.length > 0 && <Typography.Text type="secondary">点击厂商行，查看并管理它的根域、范围规则和待确认根域。</Typography.Text>}
      {cur && <ExpandPanel vendor={cur} roots={roots} onDone={() => loadDetail(cur)} />}
      {cur && (
        <Tabs
          items={[
            {
              key: 'roots',
              label: <Hint text={HELP.root}>{`根域（${(roots || []).length}）`}</Hint>,
              children: (
                <>
                  <Table
                    rowKey="id"
                    dataSource={roots || []}
                    pagination={false}
                    locale={{ emptyText: <Guide title="该厂商还没有根域" steps={['在上方厂商行点「添加根域」填 example.com 这样的顶级域名', '添加后到「扫描任务」触发 passive 开始收集子域']} /> }}
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
              label: <Hint text={HELP.scope}>{`范围（${scopes.length}）`}</Hint>,
              children: (
                <>
                <Form form={sForm} layout="inline" style={{ marginBottom: 12 }} initialValues={{ kind: 'include' }}
                  onFinish={async (vals) => {
                    try {
                      await post(`/api/vendors/${cur.id}/scopes`, vals)
                      message.success('已添加，后台重算 in_scope')
                      sForm.resetFields()
                      loadDetail(cur)
                    } catch (e) { message.error(e.message) }
                  }}>
                  <Form.Item name="kind"><Select style={{ width: 150 }} options={[{ value: 'include', label: 'include 加入范围' }, { value: 'exclude', label: 'exclude 排除' }]} /></Form.Item>
                  <Form.Item name="pattern" rules={[{ required: true, message: '填域名或 *.域名' }]}><Input placeholder="*.example.com 或 a.example.com" style={{ width: 260 }} /></Form.Item>
                  <Button htmlType="submit">添加规则</Button>
                </Form>
                <Table
                  rowKey="id"
                  dataSource={scopes}
                  pagination={false}
                  locale={{ emptyText: <Guide title="还没有范围规则，所有资产都算「不在范围内」" steps={['常用：include *.example.com 把根域下全部子域纳入', '再用 exclude 排除不测的子域，如 exclude *.cdn.example.com']} /> }}
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
                </>
              ),
            },
            {
              key: 'cands',
              label: <Hint text={HELP.candidate}>{`待确认根域（${cands.length}）`}</Hint>,
              children: (
                <Table
                  rowKey="id"
                  dataSource={cands}
                  pagination={false}
                  locale={{ emptyText: <Guide title="暂无待确认根域" steps={['填写厂商主体名后，点上方「一键发现新根域（expand）」', 'ICP 发现的新根域会出现在这里，确认后才加入扫描']} /> }}
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
    </Space>
  )
}

// T34：厂商行内添加根域。厂商已预选；校验/服务端错误就地显示在输入框下；成功后该厂商根域列表立即可见。
export function InlineRootAdd({ vendor, roots, onAdded, onClose }) {
  const [domain, setDomain] = useState('')
  const [err, setErr] = useState('')
  const [busy, setBusy] = useState(false)
  const submit = async () => {
    const d = domain.trim()
    if (!d) { setErr('请填写根域，如 example.com'); return }
    setBusy(true)
    try {
      await post(`/api/vendors/${vendor.id}/roots`, { domain: d })
      setDomain('')
      setErr('')
      await onAdded?.()
    } catch (e) {
      setErr(e.message)
    } finally {
      setBusy(false)
    }
  }
  return (
    <div data-testid={`inline-root-${vendor.id}`} onClick={(e) => e.stopPropagation()}>
      <Form layout="inline" onFinish={submit}>
        <Form.Item label={`为「${vendor.name}」添加根域`} validateStatus={err ? 'error' : ''} help={err || undefined}>
          <Input value={domain} placeholder="example.com" style={{ width: 260 }} autoFocus
            data-testid={`root-input-${vendor.id}`} onChange={(e) => { setDomain(e.target.value); setErr('') }} />
        </Form.Item>
        <Button type="primary" htmlType="submit" loading={busy} data-testid={`root-save-${vendor.id}`}>添加</Button>
        <Button style={{ marginLeft: 8 }} onClick={onClose}>收起</Button>
      </Form>
      <div style={{ marginTop: 8 }} data-testid={`root-list-${vendor.id}`}>
        <Typography.Text type="secondary">已有根域：</Typography.Text>
        {roots == null ? <Typography.Text type="secondary">加载中…</Typography.Text>
          : roots.length === 0 ? <Typography.Text type="secondary">暂无</Typography.Text>
            : roots.map((r) => <Typography.Text code key={r.id}>{r.domain}</Typography.Text>)}
      </div>
    </div>
  )
}
