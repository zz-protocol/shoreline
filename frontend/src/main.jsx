import React from 'react'
import { createRoot } from 'react-dom/client'
import { ConfigProvider, theme, App as AntApp } from 'antd'
import zhCN from 'antd/locale/zh_CN'
import { BrowserRouter } from 'react-router-dom'
import App from './App'
import 'antd/dist/reset.css'

document.documentElement.setAttribute('data-theme', 'dark')
document.body.style.background = '#141414'
document.body.style.margin = '0'

createRoot(document.getElementById('root')).render(
  <ConfigProvider
    locale={zhCN}
    theme={{
      algorithm: theme.darkAlgorithm,
      token: { colorPrimary: '#1677ff', borderRadius: 6 },
    }}
  >
    <AntApp>
      <BrowserRouter>
        <App />
      </BrowserRouter>
    </AntApp>
  </ConfigProvider>,
)
