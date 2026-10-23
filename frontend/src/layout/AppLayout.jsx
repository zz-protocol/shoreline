import React from 'react'
import { Layout, Menu, Button, Typography } from 'antd'
import { Outlet, useLocation, useNavigate } from 'react-router-dom'
import { setToken } from '../api'

const { Header, Content } = Layout
const items = [
  { key: '/events', label: '新增播报' },
  { key: '/assets', label: '资产' },
  { key: '/vendors', label: '厂商 / 根域' },
  { key: '/hosted-rules', label: '客户托管规则' },
  { key: '/scans', label: '扫描任务' },
  { key: '/probe-keys', label: '测绘 Key' },
  { key: '/tokens', label: 'API Key' },
  { key: '/docs', label: 'API 文档' },
]

// T19：顶部导航（窄屏时 Menu 自动折叠为「…」下拉）；资产/播报页表格全宽，其余页限宽 1200
const WIDE = ['/assets', '/events']

export default function AppLayout() {
  const nav = useNavigate()
  const loc = useLocation()
  const selected = items.find((i) => loc.pathname.startsWith(i.key))?.key || '/events'
  const wide = WIDE.some((p) => loc.pathname.startsWith(p)) || loc.pathname === '/'
  return (
    <Layout style={{ minHeight: '100vh' }}>
      <Header className="top-nav" style={{ display: 'flex', alignItems: 'center', gap: 32, paddingInline: 40, height: 64, borderBottom: '1px solid #1a1a1a', position: 'sticky', top: 0, zIndex: 10 }}>
        <Typography.Text style={{ fontSize: 18, fontWeight: 600, letterSpacing: 0.2, whiteSpace: 'nowrap' }}>岸线 <span style={{ color: '#737373', fontWeight: 400 }}>Shoreline</span></Typography.Text>
        <Menu theme="dark" mode="horizontal" selectedKeys={[selected]} items={items} onClick={({ key }) => nav(key)}
          style={{ flex: 1, minWidth: 0, borderBottom: 'none', background: 'transparent' }} />
        <Button type="text" onClick={() => { setToken(''); nav('/login') }} style={{ color: '#a3a3a3' }}>退出</Button>
      </Header>
      <Content className="page-content" data-wide={wide ? '1' : '0'} style={{ width: '100%', maxWidth: wide ? 'none' : 1200, margin: '0 auto', padding: '56px 40px 80px' }}>
        <Outlet />
      </Content>
    </Layout>
  )
}
