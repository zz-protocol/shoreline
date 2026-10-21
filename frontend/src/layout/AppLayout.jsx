import React from 'react'
import { Layout, Menu, Button, Typography, Space } from 'antd'
import { Outlet, useLocation, useNavigate } from 'react-router-dom'
import { setToken } from '../api'

const { Header, Sider, Content } = Layout
const items = [
  { key: '/events', label: '新增播报' },
  { key: '/assets', label: '资产' },
  { key: '/vendors', label: '厂商 / 根域' },
  { key: '/hosted-rules', label: '客户托管规则' },
  { key: '/scans', label: '扫描任务' },
  { key: '/tokens', label: 'API Key' },
  { key: '/docs', label: 'API 文档' },
]

export default function AppLayout() {
  const nav = useNavigate()
  const loc = useLocation()
  const selected = items.find((i) => loc.pathname.startsWith(i.key))?.key || '/events'
  return (
    <Layout style={{ minHeight: '100vh' }}>
      <Sider breakpoint="lg" collapsedWidth={0}>
        <div style={{ padding: 16 }}>
          <Typography.Title level={4} style={{ color: '#fff', margin: 0 }}>
            资产监控
          </Typography.Title>
        </div>
        <Menu
          theme="dark"
          mode="inline"
          selectedKeys={[selected]}
          items={items}
          onClick={({ key }) => nav(key)}
        />
      </Sider>
      <Layout>
        <Header style={{ background: 'transparent', display: 'flex', justifyContent: 'flex-end', paddingInline: 24 }}>
          <Space>
            <Button
              onClick={() => {
                setToken('')
                nav('/login')
              }}
            >
              退出
            </Button>
          </Space>
        </Header>
        <Content style={{ margin: 24 }}>
          <Outlet />
        </Content>
      </Layout>
    </Layout>
  )
}
