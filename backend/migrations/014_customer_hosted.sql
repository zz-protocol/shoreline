-- T24：客户托管排除。命中规则的资产标 customer_hosted（不删），列表/播报默认隐藏。
ALTER TABLE assets ADD COLUMN IF NOT EXISTS customer_hosted BOOLEAN NOT NULL DEFAULT false;

-- 规则表：builtin=内置；enabled=启用；deleted_at 非空为已删除（墓碑，内置规则升级时不再加回）
CREATE TABLE IF NOT EXISTS customer_hosted_rules (
    id         BIGSERIAL PRIMARY KEY,
    pattern    TEXT NOT NULL UNIQUE,
    note       TEXT NOT NULL DEFAULT '',
    builtin    BOOLEAN NOT NULL DEFAULT false,
    enabled    BOOLEAN NOT NULL DEFAULT true,
    deleted_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 改规则后的异步重算任务
CREATE TABLE IF NOT EXISTS customer_hosted_tasks (
    id          BIGSERIAL PRIMARY KEY,
    status      TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'running', 'done', 'failed')),
    processed   INT NOT NULL DEFAULT 0,
    changed     INT NOT NULL DEFAULT 0,
    error       TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at TIMESTAMPTZ
);
