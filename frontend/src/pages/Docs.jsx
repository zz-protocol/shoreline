import React, { useEffect, useRef } from 'react'
import { Typography } from 'antd'
import { getToken } from '../api'
import 'swagger-ui-dist/swagger-ui.css'

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
      <Typography.Title level={3}>API 文档（Swagger）</Typography.Title>
      <Typography.Paragraph type="secondary">
        读取 /api/openapi.yaml；试调自动带上当前登录 token。
      </Typography.Paragraph>
      <div ref={ref} />
    </div>
  )
}
