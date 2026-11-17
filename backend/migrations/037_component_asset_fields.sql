-- T60：组件-资产 8 字段。资产级 exposure/auth，组件关系级 confidence/matched_url/candidate_repo，组件级 repo_confidence/ai_review。
-- 旧行只由默认值回填（unknown / low / 空），不伪造证据；SQL 须可重跑。
ALTER TABLE assets ADD COLUMN IF NOT EXISTS exposure_state TEXT NOT NULL DEFAULT 'unknown';
ALTER TABLE assets ADD COLUMN IF NOT EXISTS auth_state TEXT NOT NULL DEFAULT 'unknown';
CREATE INDEX IF NOT EXISTS assets_exposure_state_idx ON assets (exposure_state);

ALTER TABLE url_components ADD COLUMN IF NOT EXISTS confidence TEXT NOT NULL DEFAULT 'low';
ALTER TABLE url_components ADD COLUMN IF NOT EXISTS matched_url TEXT NOT NULL DEFAULT '';
ALTER TABLE url_components ADD COLUMN IF NOT EXISTS candidate_repo TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS url_components_confidence_idx ON url_components (confidence);

ALTER TABLE asset_components ADD COLUMN IF NOT EXISTS confidence TEXT NOT NULL DEFAULT 'low';
ALTER TABLE asset_components ADD COLUMN IF NOT EXISTS matched_url TEXT NOT NULL DEFAULT '';
ALTER TABLE asset_components ADD COLUMN IF NOT EXISTS candidate_repo TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS asset_components_confidence_idx ON asset_components (confidence);

ALTER TABLE components ADD COLUMN IF NOT EXISTS repo_confidence TEXT NOT NULL DEFAULT 'low';
ALTER TABLE components ADD COLUMN IF NOT EXISTS ai_review JSONB NOT NULL DEFAULT '{}'::jsonb;
