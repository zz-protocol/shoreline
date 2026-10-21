import React, { useState } from 'react'
import { Button, Card, Form, Input, Typography, App } from 'antd'
import { useNavigate } from 'react-router-dom'
import { get, setToken } from '../api'

export default function Login() {
  const { message } = App.useApp()
  const nav = useNavigate()
  const [loading, setLoading] = useState(false)
  const onFinish = async ({ token }) => {
    setLoading(true)
    try {
      await get('/api/vendors', { token: token.trim() })
      setToken(token.trim())
      message.success('已登录')
      nav('/events')
    } catch (e) {
      message.error(e.message || 'token 无效')
    } finally {
      setLoading(false)
    }
  }
  return (
    <div style={{ minHeight: '100vh', display: 'grid', placeItems: 'center', padding: 24 }}>
      <Card title="API Token 登录" style={{ width: 420 }}>
        <Typography.Paragraph type="secondary">
          输入 Bearer token（存在 localStorage）。可用 bootstrap token 或「API Key」页创建的 token。
        </Typography.Paragraph>
        <Form layout="vertical" onFinish={onFinish}>
          <Form.Item name="token" label="Token" rules={[{ required: true, message: '请输入 token' }]}>
            <Input.Password placeholder="sam_..." autoFocus />
          </Form.Item>
          <Button type="primary" htmlType="submit" block loading={loading}>
            进入
          </Button>
        </Form>
      </Card>
    </div>
  )
}
