-- 160_agents.sql: Agent 服务实例表（每个用户最多一个实例，由 UNIQUE(user_id) 保证）
-- 用户「Agent 服务」一键部署：启动时创建用户专属 API key + 调 NY agent-manager 起容器；
-- 关闭时销毁容器 + 吊销 key + 清库。数据保留 7 天由 agent-manager 侧负责。
CREATE TABLE IF NOT EXISTS agents (
    id              BIGSERIAL PRIMARY KEY,
    user_id         BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    container_name  VARCHAR(128) NOT NULL,
    port            INT NOT NULL,
    status          VARCHAR(32) NOT NULL DEFAULT 'running',  -- running/stopped
    agent_key       TEXT NOT NULL DEFAULT '',                -- plaintext，仅用于注入容器配置；destroy 时吊销
    agent_key_id    BIGINT NOT NULL DEFAULT 0,               -- API keys 表主键，destroy 时吊销用
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_active_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (user_id)
);

CREATE INDEX IF NOT EXISTS idx_agents_user_id ON agents (user_id);
