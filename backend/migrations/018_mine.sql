-- T32：挖掘认领与回写。按「认领+结果」管理，不是布尔「挖过」。
ALTER TABLE assets ADD COLUMN IF NOT EXISTS mine_status TEXT NOT NULL DEFAULT 'unset'
    CHECK (mine_status IN ('unset', 'claimed', 'done', 'deferred', 'abandoned'));
ALTER TABLE assets ADD COLUMN IF NOT EXISTS mine_claimed_by TEXT;
ALTER TABLE assets ADD COLUMN IF NOT EXISTS mine_claimed_at TIMESTAMPTZ;
ALTER TABLE assets ADD COLUMN IF NOT EXISTS mine_claim_expires_at TIMESTAMPTZ;
ALTER TABLE assets ADD COLUMN IF NOT EXISTS mine_completed_at TIMESTAMPTZ;
ALTER TABLE assets ADD COLUMN IF NOT EXISTS mine_cooldown_until TIMESTAMPTZ;
ALTER TABLE assets ADD COLUMN IF NOT EXISTS mine_note TEXT NOT NULL DEFAULT '';
ALTER TABLE assets ADD COLUMN IF NOT EXISTS mine_report_url TEXT NOT NULL DEFAULT '';

-- 认领候选：存活、非客户托管，按状态/过期/冷却筛选时走索引
CREATE INDEX IF NOT EXISTS assets_mine_status_idx ON assets (mine_status);
CREATE INDEX IF NOT EXISTS assets_mine_claim_exp_idx ON assets (mine_claim_expires_at)
    WHERE mine_status = 'claimed';
CREATE INDEX IF NOT EXISTS assets_mine_cooldown_idx ON assets (mine_cooldown_until)
    WHERE mine_status IN ('done', 'deferred');
