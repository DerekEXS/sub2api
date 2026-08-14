package domain

// PeakWindow 分组高峰时段的单窗口配置（groups.peak_windows JSONB 数组元素）。
// 窗口语义与旧单窗口字段（peak_start/peak_end/peak_rate_multiplier）一致：
//   - 左闭右开 [Start, End)，仅支持当日区间，不支持跨天（如 22:00-次日 02:00）
//   - 判定基于全局系统时区（timezone.Location）
//
// Models 为窗口级模型白名单（空 = 组内全部模型命中；非空 = 仅白名单内模型
// 命中，支持 * 通配符后缀，如 "deepseek-v4-flash" / "deepseek-*"）。
// 用于多窗口谷峰价（如 DeepSeek 双高峰 09:00-12:00 + 14:00-18:00）下
// 只对特定模型生效的场景，避免误伤同分组其他模型。
type PeakWindow struct {
	Start      string   `json:"start"`
	End        string   `json:"end"`
	Multiplier float64  `json:"multiplier"`
	Models     []string `json:"models,omitempty"`
}
