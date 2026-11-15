-- T54：研究价值评分。四维等权（各 0.25），总分 0–100，落库两位小数；SQL 须可重跑。
ALTER TABLE components ADD COLUMN IF NOT EXISTS research_score NUMERIC(5,2) NOT NULL DEFAULT 0;
ALTER TABLE components ADD COLUMN IF NOT EXISTS research_score_parts JSONB NOT NULL DEFAULT '{}'::jsonb;
ALTER TABLE components ADD COLUMN IF NOT EXISTS research_scored_at TIMESTAMPTZ;
CREATE INDEX IF NOT EXISTS components_research_score_idx ON components (research_score DESC, id DESC);
