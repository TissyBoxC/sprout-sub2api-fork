-- 芽系列账号级模型白名单。空数组表示不额外限制；非空时由鉴权快照和
-- 网关准入共同强制。平台在创建或修改家长 AI 账号时写入该列。
ALTER TABLE users
    ADD COLUMN IF NOT EXISTS allowed_models JSONB NOT NULL DEFAULT '[]'::jsonb;

COMMENT ON COLUMN users.allowed_models IS '芽系列账户级模型白名单；空数组表示沿用分组能力，不额外限制';
