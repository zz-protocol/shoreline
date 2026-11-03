import React, { useEffect, useRef, useState } from 'react'
import { Collapse, Typography } from 'antd'
import { getToken } from '../api'
import 'swagger-ui-dist/swagger-ui.css'
import '../swagger-dark.css'

// T36：Agent 使用说明（GET /api/agent-guide，与 docs/AGENT_GUIDE.md、MCP get_agent_guide 同源）
export function AgentGuide() {
  const [text, setText] = useState(null)
  const [err, setErr] = useState('')
  useEffect(() => {
    fetch('/api/agent-guide')
      .then((r) => (r.ok ? r.text() : Promise.reject(new Error(`HTTP ${r.status}`))))
      .then(setText)
      .catch((e) => setErr(e.message))
  }, [])
  return (
    <Collapse
      defaultActiveKey={['guide']}
      style={{ marginBottom: 16 }}
      items={[{
        key: 'guide',
        label: 'Agent 使用说明（认证 / claim→heartbeat→complete / 状态 / 默认值 / 错误码 / system prompt）',
        children: err
          ? <Typography.Text type="danger">加载失败：{err}</Typography.Text>
          : (
            <Typography.Paragraph copyable={text ? { text } : false} style={{ margin: 0 }}>
              <pre data-testid="agent-guide" style={{ whiteSpace: 'pre-wrap', margin: 0, maxHeight: 600, overflow: 'auto' }}>{text ?? '加载中…'}</pre>
            </Typography.Paragraph>
          ),
      }]}
    />
  )
}

export default function Docs() {
  const ref = useRef(null)
  useEffect(() => {
    let cancelled = false
    ;(async () => {
      const mod = await import('swagger-ui-dist/swagger-ui-es-bundle.js')
      const SwaggerUIBundle = mod.default || mod.SwaggerUIBundle || mod
      if (cancelled || !ref.current) return
      SwaggerUIBundle({
        url: '/api/openapi.yaml',
        domNode: ref.current,
        persistAuthorization: true,
        requestInterceptor: (req) => {
          const tok = getToken()
          if (tok) req.headers.Authorization = `Bearer ${tok}`
          return req
        },
      })
    })()
    return () => {
      cancelled = true
      if (ref.current) ref.current.innerHTML = ''
    }
  }, [])
  return (
    <div>
      <Typography.Title level={3}>Agent 使用说明</Typography.Title>
      <AgentGuide />
      <Typography.Title level={3}>API 文档（Swagger）</Typography.Title>
      <Typography.Paragraph type="secondary">
        读取 /api/openapi.yaml；试调自动带上当前登录 token。
      </Typography.Paragraph>
      <div ref={ref} className="docs-dark" />
    </div>
  )
}
