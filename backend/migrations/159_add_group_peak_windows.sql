-- 分组多窗口高峰倍率（DeepSeek 双窗口谷峰价支持，2026-08-14）
-- 格式: [{start:"09:00", end:"12:00", multiplier:2.0, models:[]}, ...]
--   - 窗口左闭右开 [start, end)，仅支持当日区间，不支持跨天
--   - models 为窗口级模型白名单（空 = 组内全部模型命中，支持 * 通配符）
-- 旧 4 列（peak_rate_enabled/peak_start/peak_end/peak_rate_multiplier）保留兼容存量，
-- peak_windows 非空时优先于旧单窗口字段。
ALTER TABLE groups ADD COLUMN IF NOT EXISTS peak_windows JSONB;
