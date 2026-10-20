-- T8：HTTP 探测识别出的技术栈（wappalyzergo）
ALTER TABLE urls ADD COLUMN IF NOT EXISTS tech TEXT[] NOT NULL DEFAULT '{}';
