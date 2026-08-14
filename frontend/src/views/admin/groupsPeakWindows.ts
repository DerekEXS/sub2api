// 分组多窗口高峰倍率表单助手（DeepSeek 双窗口谷峰价）。
// 窗口格式与后端 groups.peak_windows JSONB 一致：
// { start: "09:00", end: "12:00", multiplier: 2.0, models: "deepseek-v4-flash,deepseek-*" }
// models 为逗号分隔字符串（空 = 组内全部模型命中），提交时序列化为数组。

export const peakWindowsI18nKey = (key: string): string =>
  `admin.groups.peakRate.${key}`;

export type PeakWindowRow = {
  id: number;
  start: string;
  end: string;
  multiplier: number;
  models: string; // 逗号分隔的模型白名单，空 = 全部模型
};

export type PeakWindowPayload = {
  start: string;
  end: string;
  multiplier: number;
  models?: string[];
};

let peakWindowRowSeq = 0;

export function createPeakWindowRow(
  start = "",
  end = "",
  multiplier = 1,
  models = "",
): PeakWindowRow {
  peakWindowRowSeq += 1;
  return { id: peakWindowRowSeq, start, end, multiplier, models };
}

// serializePeakWindows 把表单行序列化为后端 peak_windows 数组。
// 未填 start/end 或倍率为空的行被丢弃（后端另有严格校验）。
export function serializePeakWindows(rows: PeakWindowRow[]): PeakWindowPayload[] {
  const out: PeakWindowPayload[] = [];
  for (const row of rows) {
    const start = (row.start ?? "").trim();
    const end = (row.end ?? "").trim();
    if (!start || !end) {
      continue;
    }
    const multiplier = Number(row.multiplier);
    if (!Number.isFinite(multiplier) || multiplier < 0) {
      continue;
    }
    const models = (row.models ?? "")
      .split(",")
      .map((m) => m.trim())
      .filter((m) => m !== "");
    const window: PeakWindowPayload = { start, end, multiplier };
    if (models.length > 0) {
      window.models = models;
    }
    out.push(window);
  }
  return out;
}

// rowsFromPeakWindows 把后端 peak_windows 数组转换为表单行。
// 兼容旧单窗口字段：API 未返回 peak_windows 但返回 legacy 字段时，
// 由调用方以 oneLegacyRow 形式补一行，便于存量配置迁移编辑。
export function rowsFromPeakWindows(
  windows?: PeakWindowPayload[],
  oneLegacyRow?: { start: string; end: string; multiplier: number },
): PeakWindowRow[] {
  const rows: PeakWindowRow[] = [];
  if (Array.isArray(windows) && windows.length > 0) {
    for (const w of windows) {
      rows.push(
        createPeakWindowRow(
          w.start ?? "",
          w.end ?? "",
          Number.isFinite(Number(w.multiplier)) ? Number(w.multiplier) : 1,
          Array.isArray(w.models) ? w.models.join(",") : "",
        ),
      );
    }
    return rows;
  }
  if (oneLegacyRow && oneLegacyRow.start && oneLegacyRow.end) {
    rows.push(
      createPeakWindowRow(
        oneLegacyRow.start,
        oneLegacyRow.end,
        oneLegacyRow.multiplier,
        "",
      ),
    );
  }
  return rows;
}
