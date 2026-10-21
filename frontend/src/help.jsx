import React from 'react'
import { Empty, Space, Tooltip, Typography } from 'antd'
import { QuestionCircleOutlined } from '@ant-design/icons'

// 界面中文说明（T22）：名词解释 + 空状态引导，集中维护。
export const HELP = {
  vendor: '厂商：一个 SRC / 被测单位。主体名用于 ICP 备案反查（expand）。',
  activeSwitch: '主动发现开关：开启后才允许 active 扫描（字典爆破 / 变形，会向 DNS 发起查询，有限速）。默认关。',
  root: '根域：属于该厂商的顶级域名（如 example.com）。所有扫描以根域为起点，子域归属到最长匹配的根域。',
  scope: '范围：决定资产是否「在范围内」。include 加入、exclude 排除，支持 *.example.com 通配（不含 example.com 本身，需要时单独加一条）；exclude 优先。没有任何 include 规则时资产不算在范围内。改规则后会后台重算。',
  candidate: '待确认根域：expand 通过 ICP 备案 / 子公司发现的新根域。不会自动扫描，确认后才加入根域；忽略后不再提示。',
  kinds: {
    passive: 'passive 被动收集：从证书透明度、公开数据源查子域，不直接接触目标。之后自动接 dns → http。',
    active: 'active 主动发现：字典爆破、变形、递归一层，向 DNS 发查询（限速）。需厂商开启主动发现。之后自动接 dns → http。',
    expand: 'expand 扩展根域：按主体名 / 子公司 / 已有根域查 ICP 备案，结果进「待确认根域」，不自动扫描。',
    dns: 'dns 解析：链路内部步骤，解析子域并识别泛解析，不能单独触发。',
    http: 'http 探测：链路内部步骤，GET / 取状态码、标题、指纹，不能单独触发。',
  },
  degraded: '降级：外部数据源（如 ICP 服务）失败，任务仍算完成但结果可能不全，可稍后重跑。',
  hosted: '客户托管：子域名命中「客户托管规则」（云厂商分配给客户的默认域名，如 OSS/COS 桶、函数、CDN 默认域），属于托管在云上的客户资产，标记但不删除，默认在资产和播报中隐藏。',
  wildcard: '泛解析：任意子域都能解析到同一批 IP 的根域，命中的子域标为泛解析，默认隐藏、不播报。',
  alive: '探活：DNS 能解析且 http/https 任一有响应（任何状态码）即「存活」，确认后不再复查；DNS 失败或无响应为「失活」，按 SCHEDULE_ALIVE（默认 24h）定时复查，恢复时播报一条「恢复」；未探测或确认为泛解析为「未知」。',
  level: '资产级：新出现的子域；URL 级：新出现的 http(s) 站点。资产级事件下会挂上它的 URL 事件。',
}

/** 文字 + 问号提示。 */
export function Hint({ text, children }) {
  return (
    <Space size={4}>
      {children}
      <Tooltip title={text}>
        <QuestionCircleOutlined style={{ color: 'rgba(255,255,255,0.45)', cursor: 'help' }} aria-label={text} />
      </Tooltip>
    </Space>
  )
}

/** 空状态：说明 + 下一步引导。 */
export function Guide({ title, steps }) {
  return (
    <Empty
      image={Empty.PRESENTED_IMAGE_SIMPLE}
      description={
        <Space direction="vertical" size={2}>
          <Typography.Text>{title}</Typography.Text>
          {steps.map((s, i) => (
            <Typography.Text key={i} type="secondary">{s}</Typography.Text>
          ))}
        </Space>
      }
    />
  )
}
