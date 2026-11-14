-- T46：未识别 URL 的 LLM 候选。只加列，不调用模型、不对外请求。
ALTER TABLE urls ADD COLUMN IF NOT EXISTS llm_evidence_hash TEXT;
ALTER TABLE url_components ADD COLUMN IF NOT EXISTS ignored_at TIMESTAMPTZ;
