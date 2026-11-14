import React, { useEffect, useState } from 'react'
import { App, Button, Drawer, Input, InputNumber, Modal, Space, Switch, Table, Tag, Typography } from 'antd'
import { CopyOutlined } from '@ant-design/icons'
import { del, get, patch, post } from '../api'
import { Guide, HELP, Hint } from '../help'
import { copyText } from '../clipboard'

export default function Components() {
  const { message } = App.useApp()
  const [q, setQ] = useState('')
  const [minAssets, setMinAssets] = useState()
  const [minVendors, setMinVendors] = useState()
  const [excludeCommon, setExcludeCommon] = useState(true)
  const [wild, setWild] = useState(false)
  const [hosted, setHosted] = useState(false)
  const [dead, setDead] = useState(false)
  const [items, setItems] = useState([])
  const [cursor, setCursor] = useState(null)
  const [loading, setLoading] = useState(false)
  const [manual, setManual] = useState(null)
  const [detail, setDetail] = useState(null)
  const [assets, setAssets] = useState([])
  const [assetCursor, setAssetCursor] = useState(null)
  const [assetLoading, setAssetLoading] = useState(false)
  const [vendor, setVendor] = useState()
  const [vendors, setVendors] = useState([])
  const [commonOpen, setCommonOpen] = useState(false)
  const [common, setCommon] = useState([])
  const [newCommon, setNewCommon] = useState('')
  const [maxStars, setMaxStars] = useState()
  const [maxAdvisories, setMaxAdvisories] = useState()
  const [missingRepo, setMissingRepo] = useState(false)
  const [sort, setSort] = useState('')
  const [repoDraft, setRepoDraft] = useState('')

  useEffect(() => {
    get('/api/vendors').then((d) => setVendors(d.items || [])).catch((e) => message.error(e.message))
  }, [])

  const filters = () => {
    const p = new URLSearchParams({
      exclude_common: String(!!excludeCommon),
      include_wildcard: String(!!wild),
      include_customer_hosted: String(!!hosted),
      include_dead: String(!!dead),
    })
    if (q?.trim()) p.set('q', q.trim())
    if (minAssets != null && minAssets !== '') p.set('min_assets', String(minAssets))
    if (minVendors != null && minVendors !== '') p.set('min_vendors', String(minVendors))
    if (maxStars != null && maxStars !== '') p.set('max_stars', String(maxStars))
    if (maxAdvisories != null && maxAdvisories !== '') p.set('max_advisories', String(maxAdvisories))
    if (missingRepo) p.set('missing_repo', 'true')
    if (sort) p.set('sort', sort)
    return p
  }

  const assetFilters = () => {
    const p = new URLSearchParams({
      include_wildcard: String(!!wild),
      include_customer_hosted: String(!!hosted),
      include_dead: String(!!dead),
    })
    if (vendor) p.set('vendor', String(vendor))
    return p
  }

  const load = async (next) => {
    setLoading(true)
    try {
      const params = filters()
      params.set('limit', '50')
      if (next) params.set('cursor', next)
      const d = await get(`/api/components?${params}`)
      setItems(next ? [...items, ...(d.items || [])] : (d.items || []))
      setCursor(d.next_cursor || null)
    } catch (e) {
      message.error(e.message)
    } finally {
      setLoading(false)
    }
  }
  useEffect(() => { load() }, [excludeCommon, wild, hosted, dead, minAssets, minVendors, maxStars, maxAdvisories, missingRepo, sort])

  const loadAssets = async (comp, next, vendorId = vendor) => {
    setAssetLoading(true)
    try {
      const p = new URLSearchParams({
        include_wildcard: String(!!wild),
        include_customer_hosted: String(!!hosted),
        include_dead: String(!!dead),
        limit: '50',
      })
      if (vendorId) p.set('vendor', String(vendorId))
      if (next) p.set('cursor', next)
      const d = await get(`/api/components/${comp.id}/assets?${p}`)
      setAssets(next ? [...assets, ...(d.items || [])] : (d.items || []))
      setAssetCursor(d.next_cursor || null)
    } catch (e) {
      message.error(e.message)
    } finally {
      setAssetLoading(false)
    }
  }

  const openDetail = (row) => {
    setDetail(row)
    setRepoDraft(row.github_repo || '')
    setVendor(undefined)
    setAssets([])
    setAssetCursor(null)
    loadAssets(row, null, undefined)
  }

  const saveRepo = async () => {
    if (!detail) return
    try {
      const d = await patch(`/api/components/${detail.id}`, { github_repo: repoDraft })
      setDetail({ ...detail, ...d })
      setRepoDraft(d.github_repo || '')
      load()
      message.success('已保存仓库')
    } catch (e) {
      message.error(e.message)
    }
  }

  const copyComp = async (field) => {
    if (!detail) return
    try {
      const p = assetFilters()
      p.set('field', field)
      const d = await get(`/api/components/${detail.id}/values?${p}`)
      const text = (d.items || []).join('\n')
      if (await copyText(text)) {
        message.success(`已复制 ${d.count} 条${d.truncated ? '（已截断至上限）' : ''}`)
      } else {
        setManual({ text, count: d.count, truncated: d.truncated })
      }
    } catch (e) {
      message.error(e.message)
    }
  }

  const loadCommon = async () => {
    try {
      const d = await get('/api/common-components')
      setCommon(d.items || [])
    } catch (e) {
      message.error(e.message)
    }
  }

  return (
    <Space direction="vertical" style={{ width: '100%' }} size="large">
      <Space style={{ width: '100%', justifyContent: 'space-between' }}>
        <Typography.Title level={3} style={{ margin: 0 }}>组件</Typography.Title>
        <Button onClick={() => { setCommonOpen(true); loadCommon() }} data-testid="manage-common">管理常见名单</Button>
      </Space>
      <Space wrap>
        <Input.Search placeholder="组件名" value={q} onChange={(e) => setQ(e.target.value)} onSearch={() => load()} style={{ width: 200 }} data-testid="comp-q" />
        <InputNumber min={0} placeholder="最小资产数" value={minAssets} onChange={setMinAssets} data-testid="min-assets" />
        <InputNumber min={0} placeholder="最小厂商数" value={minVendors} onChange={setMinVendors} data-testid="min-vendors" />
        <span>
          <Hint text={HELP.commonComponents}>排除常见</Hint> <Switch checked={excludeCommon} onChange={setExcludeCommon} data-testid="exclude-common" />
        </span>
        <span>
          <Hint text={HELP.wildcard}>含泛解析</Hint> <Switch checked={wild} onChange={setWild} data-testid="comp-wild" />
        </span>
        <span>
          <Hint text={HELP.hosted}>含客户托管</Hint> <Switch checked={hosted} onChange={setHosted} data-testid="comp-hosted" />
        </span>
        <span>
          <Hint text={HELP.alive}>含失活</Hint> <Switch checked={dead} onChange={setDead} data-testid="comp-dead" />
        </span>
        <InputNumber min={0} placeholder="最多 star" value={maxStars} onChange={setMaxStars} data-testid="max-stars" />
        <InputNumber min={0} placeholder="最多公开披露数" value={maxAdvisories} onChange={setMaxAdvisories} data-testid="max-advisories" />
        <span>
          未关联仓库 <Switch checked={missingRepo} onChange={setMissingRepo} data-testid="missing-repo" />
        </span>
        <select data-testid="comp-sort" value={sort} onChange={(e) => setSort(e.target.value)} style={{ height: 32 }}>
          <option value="">默认排序（厂商数）</option>
          <option value="stars">Star 升序</option>
          <option value="advisories">公开披露数升序</option>
        </select>
      </Space>
      <Typography.Text type="secondary">{HELP.components}</Typography.Text>
      <Typography.Text type="secondary" data-testid="advisory-note">{HELP.advisories}</Typography.Text>
      <Table
        rowKey="id"
        loading={loading}
        dataSource={items}
        pagination={false}
        onRow={(row) => ({ onClick: () => openDetail(row), style: { cursor: 'pointer' }, 'data-testid': `comp-row-${row.id}` })}
        locale={{ emptyText: <Guide title="没有组件" steps={['先完成 http 扫描，或升级后从已有技术栈回填', '默认会排除常见组件，可关闭「排除常见」查看全部']} /> }}
        columns={[
          { title: '组件名', dataIndex: 'name' },
          { title: '来源', dataIndex: 'source', width: 140 },
          { title: 'URL 数', dataIndex: 'url_count', width: 100 },
          { title: '资产数', dataIndex: 'asset_count', width: 100 },
          { title: '厂商数', dataIndex: 'vendor_count', width: 100 },
          { title: 'GitHub 仓库', dataIndex: 'github_repo', width: 160, render: (v) => v || '—' },
          { title: 'Star', dataIndex: 'stars', width: 80, render: (v) => (v == null ? '—' : v) },
          { title: '公开披露数', dataIndex: 'advisory_count', width: 120, render: (v) => (v == null ? '—' : v) },
          { title: '是否常见', dataIndex: 'is_common', width: 100, render: (v) => (v ? <Tag data-testid="common-tag">常见</Tag> : '') },
        ]}
      />
      {cursor && <Button onClick={() => load(cursor)} data-testid="comp-more">加载更多</Button>}

      <Drawer title={detail ? detail.name : ''} open={!!detail} width={720} onClose={() => setDetail(null)} destroyOnClose>
        {detail && (
          <Space direction="vertical" style={{ width: '100%' }} size="middle">
            <Space wrap>
              <Input placeholder="owner/repo" value={repoDraft} onChange={(e) => setRepoDraft(e.target.value)} data-testid="github-repo" style={{ width: 240 }} />
              <Button type="primary" data-testid="save-repo" onClick={saveRepo}>保存仓库</Button>
              <Button data-testid="copy-comp-subdomain" icon={<CopyOutlined />} onClick={() => copyComp('subdomain')}>复制本组件资产主机名</Button>
              <Button data-testid="copy-comp-url" icon={<CopyOutlined />} onClick={() => copyComp('url')}>复制 URL</Button>
              <select data-testid="comp-vendor" value={vendor || ''} onChange={(e) => {
                const v = e.target.value ? Number(e.target.value) : undefined
                setVendor(v)
                loadAssets(detail, null, v)
              }} style={{ height: 32 }}>
                <option value="">全部厂商</option>
                {vendors.map((v) => <option key={v.id} value={v.id}>{v.name}</option>)}
              </select>
            </Space>
            {detail.enrich_error && <Typography.Text type="danger" data-testid="enrich-error">富集失败：{detail.enrich_error}</Typography.Text>}
            <Typography.Text type="secondary">复制覆盖当前筛选下的全部结果（非仅本页），一行一个、去重；HTTP 下自动回退，失败可手动复制。</Typography.Text>
            <Table
              rowKey="id"
              loading={assetLoading}
              dataSource={assets}
              pagination={false}
              size="small"
              columns={[
                { title: '子域', dataIndex: 'subdomain' },
                { title: '厂商', dataIndex: 'vendor', width: 140 },
                { title: 'URL', dataIndex: 'urls', render: (us) => <span style={{ whiteSpace: 'pre-wrap' }}>{(us || []).map((u) => u.url).join('\n')}</span> },
              ]}
            />
            {assetCursor && <Button onClick={() => loadAssets(detail, assetCursor)} data-testid="comp-assets-more">加载更多</Button>}
          </Space>
        )}
      </Drawer>

      <Modal title="常见组件名单" open={commonOpen} footer={null} onCancel={() => setCommonOpen(false)} width={560}>
        <Hint text={HELP.commonComponents}><span /></Hint>
        <Space style={{ marginBottom: 12 }} wrap>
          <Input placeholder="组件名（与识别名对齐）" value={newCommon} onChange={(e) => setNewCommon(e.target.value)} data-testid="common-name" style={{ width: 240 }} />
          <Button type="primary" data-testid="common-add" onClick={async () => {
            try {
              await post('/api/common-components', { name: newCommon })
              setNewCommon('')
              await loadCommon()
              load()
            } catch (e) { message.error(e.message) }
          }}>新增</Button>
        </Space>
        <Table
          rowKey="id"
          size="small"
          dataSource={common}
          pagination={false}
          columns={[
            { title: '名称', dataIndex: 'name' },
            { title: '内置', dataIndex: 'builtin', width: 80, render: (v) => (v ? '是' : '否') },
            { title: '启用', dataIndex: 'enabled', width: 80, render: (v, r) => (
              <Switch checked={v} data-testid={`common-enable-${r.id}`} onChange={async (on) => {
                try { await patch(`/api/common-components/${r.id}`, { enabled: on }); await loadCommon(); load() } catch (e) { message.error(e.message) }
              }} />
            ) },
            { title: '', width: 80, render: (_, r) => (
              <Button size="small" danger data-testid={`common-del-${r.id}`} onClick={async () => {
                try { await del(`/api/common-components/${r.id}`); await loadCommon(); load() } catch (e) { message.error(e.message) }
              }}>删除</Button>
            ) },
          ]}
        />
      </Modal>

      <Modal open={!!manual} title={`浏览器不允许自动复制，请手动复制（${manual?.count ?? 0} 条${manual?.truncated ? '，已截断至上限' : ''}）`} footer={null} onCancel={() => setManual(null)}>
        <Input.TextArea value={manual?.text || ''} rows={12} readOnly data-testid="manual-copy" />
      </Modal>
    </Space>
  )
}
