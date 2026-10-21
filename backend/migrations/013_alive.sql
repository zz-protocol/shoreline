-- T21：资产探活。alive / dead / unknown；一旦 alive 不再改回。回填只改状态，不产生事件。
ALTER TABLE assets ADD COLUMN IF NOT EXISTS alive TEXT NOT NULL DEFAULT 'unknown'
    CHECK (alive IN ('alive', 'dead', 'unknown'));
ALTER TABLE assets ADD COLUMN IF NOT EXISTS last_alive_at TIMESTAMPTZ;
UPDATE assets a SET alive = 'alive', last_alive_at = (SELECT max(u.last_seen) FROM urls u WHERE u.asset_id = a.id)
    WHERE a.resolved AND EXISTS (SELECT 1 FROM urls u WHERE u.asset_id = a.id);
CREATE INDEX IF NOT EXISTS assets_dead_idx ON assets (root_id) WHERE alive = 'dead';
