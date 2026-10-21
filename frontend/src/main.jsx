import React from 'react'
import { createRoot } from 'react-dom/client'
import { ConfigProvider, theme, App as AntApp } from 'antd'
import zhCN from 'antd/locale/zh_CN'
import { BrowserRouter } from 'react-router-dom'
import App from './App'
import 'antd/dist/reset.css'
import './design.css'

document.documentElement.setAttribute('data-theme', 'dark')
document.body.style.background = '#000'
document.body.style.margin = '0'

createRoot(document.getElementById('root')).render(
  <ConfigProvider
    locale={zhCN}
    theme={{
      algorithm: theme.darkAlgorithm,
      // T19 设计稿：grok.com 风格——纯黑底、白色主色、低饱和灰阶、高对比文字、大圆角
      token: {
        colorPrimary: '#fafafa',
        colorInfo: '#60a5fa',
        colorSuccess: '#4ade80',
        colorWarning: '#fbbf24',
        colorError: '#f87171',
        colorBgBase: '#000000',
        colorBgLayout: '#000000',
        colorBgContainer: '#0a0a0a',
        colorBgElevated: '#141414',
        colorBorder: '#262626',
        colorBorderSecondary: '#1a1a1a',
        colorText: '#f5f5f5',
        colorTextSecondary: '#a3a3a3',
        colorTextTertiary: '#737373',
        borderRadius: 10,
        fontSize: 14,
        fontFamily: "Inter, -apple-system, 'Segoe UI', 'PingFang SC', 'Microsoft YaHei', sans-serif",
      },
      components: {
        Button: { primaryColor: '#000000', borderRadius: 999, controlHeight: 34, fontWeight: 500 },
        Layout: { headerBg: '#000000', bodyBg: '#000000', siderBg: '#000000' },
        Menu: { darkItemBg: '#000000', darkPopupBg: '#141414', darkSubMenuItemBg: '#141414', darkItemSelectedBg: '#1a1a1a', darkItemSelectedColor: '#ffffff', darkItemColor: '#a3a3a3', itemBorderRadius: 999 },
        Table: { headerBg: '#000000', headerColor: '#737373', rowHoverBg: '#111111', borderColor: '#1a1a1a', headerSplitColor: 'transparent' },
        Switch: { colorPrimary: '#60a5fa', colorPrimaryHover: '#93c5fd' },
        Tag: { defaultBg: '#141414' },
        Card: { colorBgContainer: '#0a0a0a' },
      },
    }}
  >
    <AntApp>
      <BrowserRouter>
        <App />
      </BrowserRouter>
    </AntApp>
  </ConfigProvider>,
)
