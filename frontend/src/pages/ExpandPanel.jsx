import React, { useEffect, useRef, useState } from 'react'
import { Alert, Button, Card, Space, Tag, Typography } from 'antd'
import { ThunderboltOutlined } from '@ant-design/icons'
import dayjs from 'dayjs'
import { get, post } from '../api'

export const EXPAND_STATUS = {
  pending: { label: '排队中', color: 'default' },
  running: { label: '运行中', color: 'processing' },
  retrying: { label: '重试中', color: 'warning' },
  done: { label: '已完成', color: 'success' },
  failed: { label: '失败', color: 'error' },
}
const active = (s) => s === 'pending' || s === 'running' || s === 'retrying'

/**
 * T23 一键 expand：对厂商跑 ICP/子公司扩展（expand 按任务所属根域的厂商整体跑，任选一个根域触发即可），
 * 结果进「待确认根域」；显示最近一次 expand 的运行状态与结果，运行中每 3 秒刷新，结束后回调 onDone。
 */
export default function ExpandPanel({ vendor, roots, onDone, interval = 3000 }) {
  const [last, setLast] = useState(null)
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState('')
  const prev = useRef(null)
  const vid = useRef(vendor.id) // 当前厂商：迟到的旧厂商响应据此丢弃
  const seq = useRef(0) // 最新一次 load 的序号：较早发出但较晚返回的响应丢弃
  const inflight = useRef(false) // 同步守卫：连点只发一个 POST

  const load = async () => {
    const id = vendor.id
    const n = ++seq.current
    try {
      const d = await get(`/api/scans?vendor=${id}&kind=expand&limit=1`)
      if (vid.current !== id || seq.current !== n) return
      const s = d.items?.[0] || null
      if (prev.current && active(prev.current.status) && s && !active(s.status)) onDone?.()
      prev.current = s
      setLast(s)
    } catch (e) {
      if (vid.current === id && seq.current === n) setErr(e.message)
    }
  }
  useEffect(() => {
    vid.current = vendor.id
    prev.current = null
    setLast(null)
    setErr('')
    load()
  }, [vendor.id])
  useEffect(() => {
    if (!last || !active(last.status)) return undefined
    const t = setInterval(load, interval)
    return () => clearInterval(t)
  }, [last?.id, last?.status])

  const rootsReady = Array.isArray(roots)
  const root = rootsReady ? roots.find((r) => r.vendor_id == null || r.vendor_id === vendor.id) : null

  const run = async () => {
    if (inflight.current) return
    if (!root || (root.vendor_id != null && root.vendor_id !== vendor.id)) {
      setErr('根域尚未加载或不属于当前厂商，请稍后重试')
      return
    }
    inflight.current = true
    const id = vendor.id
    setBusy(true)
    setErr('')
    try {
      await post('/api/scans', { root_id: root.id, kind: 'expand' })
    } catch (e) {
      if (vid.current === id) setErr(e.status === 409 ? '该厂商已有 expand 在进行' : e.message)
    } finally {
      inflight.current = false
      setBusy(false)
    }
    if (vid.current === id) await load() // 成功或 409 都拉取最近（正在跑的）任务显示状态
  }

  const st = last && (EXPAND_STATUS[last.status] || { label: last.status })
  const r = last?.result || {}
  const noRoot = rootsReady && !root
  return (
    <Card size="small" data-testid="expand-panel">
      <Space direction="vertical" style={{ width: '100%' }}>
        <Space wrap>
          <Button type="primary" size="large" icon={<ThunderboltOutlined />} loading={busy}
            disabled={!rootsReady || noRoot || busy || active(last?.status)} onClick={run} data-testid="expand-btn">
            一键发现新根域（expand）
          </Button>
          <Typography.Text type="secondary">
            按主体名{vendor.company_name ? `「${vendor.company_name}」` : '（未填写）'}、持股 ≥50% 的子公司和已有根域查 ICP 备案，新根域进「待确认根域」，确认后才加入扫描。
          </Typography.Text>
        </Space>
        {noRoot && <Alert type="info" showIcon message="该厂商还没有根域：expand 任务需挂在一个根域上，请先添加根域" />}
        {err && <Alert type="error" showIcon message={err} />}
        {last ? (
          <Space wrap data-testid="expand-status">
            <span>最近一次：</span>
            <Tag color={st.color}>{st.label}</Tag>
            <span>#{last.id} · {dayjs(last.created_at).format('YYYY-MM-DD HH:mm:ss')}</span>
            {last.status === 'done' && <span>发现 {r.found ?? 0} 个，新增待确认 {r.new_candidates ?? 0} 个</span>}
            {last.degraded && <Tag color="warning">降级：{last.degraded_reason}</Tag>}
            {last.error && <Typography.Text type="danger">{last.error}</Typography.Text>}
          </Space>
        ) : (
          <Typography.Text type="secondary">还没有跑过 expand</Typography.Text>
        )}
      </Space>
    </Card>
  )
}
