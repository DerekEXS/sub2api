package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// === task #321：汇率 API 机制修复单测（2026-08-16）===

func newTestFXService() *FXService {
	s := &FXService{
		cache:      fxCacheFile{Entries: map[string]fxCacheEntry{}},
		cachePath:  "",
		log:        nil,
		httpClient: &http.Client{Timeout: fxHTTPTimeout},
		clock:      time.Now,
	}
	return s
}

// fxTestServer 返回一个按 URL 路径分发响应的测试服务器。
func fxTestServer(t *testing.T, handlers map[string]http.HandlerFunc) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	for path, h := range handlers {
		mux.HandleFunc(path, h)
	}
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func fxJSON(t *testing.T, v any) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
}

// --- fetchAPI 解析 ---

func TestFXService_fetchAPI_exchangerateV4Format(t *testing.T) {
	ts := fxTestServer(t, map[string]http.HandlerFunc{
		"/v4/latest/USD": fxJSON(t, map[string]any{
			"base":  "USD",
			"date":  "2026-08-16",
			"rates": map[string]float64{"USD": 1, "CNY": 6.76},
		}),
	})
	s := newTestFXService()
	rates, base, tsTime, ok := s.fetchAPI(context.Background(), ts.URL+"/v4/latest/USD")
	require.True(t, ok)
	require.Equal(t, "USD", base)
	require.Equal(t, 6.76, rates["CNY"])
	require.False(t, tsTime.IsZero())
	require.Equal(t, 2026, tsTime.Year())
}

func TestFXService_fetchAPI_openERAPIV6Format(t *testing.T) {
	// open.er-api v6：base_code + result + time_last_update_unix（不含 base 字段）
	ts := fxTestServer(t, map[string]http.HandlerFunc{
		"/v6/latest/USD": fxJSON(t, map[string]any{
			"result":                "success",
			"base_code":             "USD",
			"time_last_update_unix": 1786838551,
			"rates":                 map[string]float64{"USD": 1, "CNY": 6.760612},
		}),
	})
	s := newTestFXService()
	rates, base, tsTime, ok := s.fetchAPI(context.Background(), ts.URL+"/v6/latest/USD")
	require.True(t, ok)
	require.Equal(t, "USD", base)
	require.InDelta(t, 6.760612, rates["CNY"], 1e-9)
	require.Equal(t, time.Unix(1786838551, 0).UTC(), tsTime)
}

func TestFXService_fetchAPI_resultErrorSkipped(t *testing.T) {
	// open.er-api 错误响应：{"result":"error","error-type":"..."}，无 rates → 应跳过
	ts := fxTestServer(t, map[string]http.HandlerFunc{
		"/latest": fxJSON(t, map[string]any{
			"result":     "error",
			"error-type": "invalid-key",
		}),
	})
	s := newTestFXService()
	_, _, _, ok := s.fetchAPI(context.Background(), ts.URL+"/latest")
	require.False(t, ok)
}

func TestFXService_fetchAPI_garbageSkipped(t *testing.T) {
	// 源返回格式变化（HTML/非 JSON/空 rates）→ 跳过而非用错误值
	ts := fxTestServer(t, map[string]http.HandlerFunc{
		"/html": func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("<html>maintenance</html>"))
		},
		"/empty-rates": fxJSON(t, map[string]any{"base": "USD", "rates": map[string]float64{}}),
		"/bad-json": func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"base": "USD", `))
		},
		"/non-2xx": func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "denied", http.StatusForbidden)
		},
	})
	s := newTestFXService()
	for _, path := range []string{"/html", "/empty-rates", "/bad-json", "/non-2xx"} {
		_, _, _, ok := s.fetchAPI(context.Background(), ts.URL+path)
		require.False(t, ok, "path %s should be skipped", path)
	}
}

func TestFXService_parseFxTimestamp(t *testing.T) {
	// YYYY-MM-DD（exchangerate-api v4 / frankfurter）
	require.Equal(t, time.Date(2026, 8, 16, 0, 0, 0, 0, time.UTC),
		parseFxTimestamp(&fxAPIResponse{Date: "2026-08-16"}))
	// RFC3339（fxratesapi）
	require.Equal(t, time.Date(2026, 8, 16, 10, 22, 0, 0, time.UTC),
		parseFxTimestamp(&fxAPIResponse{Date: "2026-08-16T10:22:00.000Z"}))
	// RFC1123（open.er-api time_last_update_utc）
	require.Equal(t, time.Date(2026, 8, 16, 0, 2, 31, 0, time.UTC),
		parseFxTimestamp(&fxAPIResponse{TimeLastUpdateUTC: "Sun, 16 Aug 2026 00:02:31 +0000"}))
	// unix 秒优先
	require.Equal(t, time.Unix(1786838551, 0).UTC(),
		parseFxTimestamp(&fxAPIResponse{Date: "2026-08-16", TimeLastUpdateUnix: 1786838551}))
	// 无法解析 → 零值
	require.True(t, parseFxTimestamp(&fxAPIResponse{Date: "not-a-date"}).IsZero())
	require.True(t, parseFxTimestamp(&fxAPIResponse{}).IsZero())
}

// --- GetRate 回退链 ---

func TestFXService_GetRate_chainFirstFreshWins(t *testing.T) {
	// 链：URL1 失效 → URL2 新鲜成功
	ts := fxTestServer(t, map[string]http.HandlerFunc{
		"/dead": func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "down", http.StatusServiceUnavailable)
		},
		"/ok": fxJSON(t, map[string]any{
			"base":  "USD",
			"date":  time.Now().UTC().Format("2006-01-02"),
			"rates": map[string]float64{"USD": 1, "CNY": 6.75},
		}),
	})
	s := newTestFXService()
	rate := s.GetRate(context.Background(), "USD", "CNY",
		[]string{ts.URL + "/dead", ts.URL + "/ok"}, 6.9)
	require.InDelta(t, 6.75, rate, 1e-9)
	// 结果应写入缓存（新鲜数据）
	entry, ok := s.cache.Entries[cacheKey("USD", "CNY")]
	require.True(t, ok)
	require.InDelta(t, 6.75, entry.Rate, 1e-9)
	require.Equal(t, 0, s.consecutiveFail)
}

func TestFXService_GetRate_staleSkippedWhenFreshExists(t *testing.T) {
	// 链：URL1 数据过期（10 天前）→ 应跳过；URL2 新鲜 → 使用 URL2
	ts := fxTestServer(t, map[string]http.HandlerFunc{
		"/stale": fxJSON(t, map[string]any{
			"base":  "USD",
			"date":  time.Now().UTC().Add(-10 * 24 * time.Hour).Format("2006-01-02"),
			"rates": map[string]float64{"USD": 1, "CNY": 9.99}, // 过期源的错误值绝不能被用
		}),
		"/fresh": fxJSON(t, map[string]any{
			"base":  "USD",
			"date":  time.Now().UTC().Format("2006-01-02"),
			"rates": map[string]float64{"USD": 1, "CNY": 6.74},
		}),
	})
	s := newTestFXService()
	rate := s.GetRate(context.Background(), "USD", "CNY",
		[]string{ts.URL + "/stale", ts.URL + "/fresh"}, 6.9)
	require.InDelta(t, 6.74, rate, 1e-9)
	entry, ok := s.cache.Entries[cacheKey("USD", "CNY")]
	require.True(t, ok)
	require.InDelta(t, 6.74, entry.Rate, 1e-9)
}

func TestFXService_GetRate_allStaleUsesStaleAsLastResort(t *testing.T) {
	// 全部来源都过期 → 用第一个过期值兜底（优于固定汇率），且不写缓存
	ts := fxTestServer(t, map[string]http.HandlerFunc{
		"/stale": fxJSON(t, map[string]any{
			"base":  "USD",
			"date":  time.Now().UTC().Add(-9 * 24 * time.Hour).Format("2006-01-02"),
			"rates": map[string]float64{"USD": 1, "CNY": 6.71},
		}),
	})
	s := newTestFXService()
	rate := s.GetRate(context.Background(), "USD", "CNY", []string{ts.URL + "/stale"}, 6.9)
	require.InDelta(t, 6.71, rate, 1e-9)
	_, ok := s.cache.Entries[cacheKey("USD", "CNY")]
	require.False(t, ok, "stale rate must not be cached")
}

func TestFXService_GetRate_allFailFallback(t *testing.T) {
	ts := fxTestServer(t, map[string]http.HandlerFunc{
		"/dead": func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "down", http.StatusServiceUnavailable)
		},
	})
	s := newTestFXService()
	rate := s.GetRate(context.Background(), "USD", "CNY", []string{ts.URL + "/dead"}, 6.9)
	require.InDelta(t, 6.9, rate, 1e-9)
	// 连续失败计数上升（1 次）
	require.Equal(t, 1, s.consecutiveFail)
	// 反向换算（第二次调用再失败 1 次）
	rate = s.GetRate(context.Background(), "CNY", "USD", []string{ts.URL + "/dead"}, 6.9)
	require.InDelta(t, 1/6.9, rate, 1e-9)
	require.Equal(t, 2, s.consecutiveFail)
}

func TestFXService_GetRate_defaultChainWhenEmpty(t *testing.T) {
	// 未配置 URL → 走内置默认链（多源）；此处用同源服务验证逻辑分支进入默认链
	old := defaultFXAPIURLs
	defer func() { defaultFXAPIURLs = old }()
	ts := fxTestServer(t, map[string]http.HandlerFunc{
		"/ok": fxJSON(t, map[string]any{
			"base":  "USD",
			"date":  time.Now().UTC().Format("2006-01-02"),
			"rates": map[string]float64{"USD": 1, "CNY": 6.73},
		}),
	})
	defaultFXAPIURLs = []string{ts.URL + "/ok"}
	s := newTestFXService()
	rate := s.GetRate(context.Background(), "USD", "CNY", nil, 6.9)
	require.InDelta(t, 6.73, rate, 1e-9)
	// fxAPICandidates 同样回退到默认链
	require.Equal(t, defaultFXAPIURLs, fxAPICandidates(nil))
}

func TestFXService_GetRate_sameCurrencyAndAbnormal(t *testing.T) {
	// 覆盖默认链为本地服务，避免测试依赖外网
	old := defaultFXAPIURLs
	defer func() { defaultFXAPIURLs = old }()
	ts := fxTestServer(t, map[string]http.HandlerFunc{
		"/dead": func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "down", http.StatusServiceUnavailable)
		},
	})
	defaultFXAPIURLs = []string{ts.URL + "/dead"}
	s := newTestFXService()
	require.Equal(t, 1.0, s.GetRate(context.Background(), "USD", "USD", nil, 6.9))
	require.Equal(t, 0.0, s.GetRate(context.Background(), "", "CNY", nil, 6.9))
	// NaN/Inf/<=0 fallbackRate → 归一为默认；URL 全失败 → fallback
	require.InDelta(t, defaultFXFallbackRate, s.GetRate(context.Background(), "USD", "CNY", nil, 0), 1e-9)
}

func TestFXService_GetRate_missingPairSkipped(t *testing.T) {
	// 源缺对币（如只有 USD/EUR 没有 CNY）→ 跳过该源 → 兜底
	ts := fxTestServer(t, map[string]http.HandlerFunc{
		"/no-cny": fxJSON(t, map[string]any{
			"base":  "USD",
			"date":  time.Now().UTC().Format("2006-01-02"),
			"rates": map[string]float64{"USD": 1, "EUR": 0.86},
		}),
	})
	s := newTestFXService()
	rate := s.GetRate(context.Background(), "USD", "CNY", []string{ts.URL + "/no-cny"}, 6.9)
	require.InDelta(t, 6.9, rate, 1e-9)
}

// --- TestFetchAPI / 数学 ---

func TestFXService_TestFetchAPI(t *testing.T) {
	ts := fxTestServer(t, map[string]http.HandlerFunc{
		"/ok": fxJSON(t, map[string]any{
			"result":    "success",
			"base_code": "USD",
			"rates":     map[string]float64{"USD": 1, "CNY": 6.760612},
			"date":      "2026-08-16",
		}),
	})
	s := newTestFXService()
	rate, base, ok := s.TestFetchAPI(ts.URL + "/ok")
	require.True(t, ok)
	require.Equal(t, "USD", base)
	require.InDelta(t, 6.760612, rate, 1e-9)
	_, _, ok = s.TestFetchAPI("not-a-url")
	require.False(t, ok)
}

func TestFXService_ConvertAmountAndSanity(t *testing.T) {
	s := newTestFXService()
	require.InDelta(t, 67.6, s.ConvertAmount(10, "USD", "CNY", 6.76), 1e-9)
	require.Equal(t, 0.0, s.ConvertAmount(0, "USD", "CNY", 6.76))
	require.Equal(t, 0.0, s.ConvertAmount(10, "USD", "CNY", 0))
	require.NoError(t, SanityCheck("USD", "CNY", 6.76))
	require.Error(t, SanityCheck("USD", "USD", 6.76))
	require.Error(t, SanityCheck("USD", "CNY", -1))
	require.Equal(t, "6.7600", FormatRateForDisplay(6.76))
	require.Equal(t, "—", FormatRateForDisplay(0))
}

func TestFXService_fallbackCrossViaUSD(t *testing.T) {
	require.InDelta(t, 6.9, fallbackCrossViaUSD("USD", "CNY", 6.9), 1e-9)
	require.InDelta(t, 1/6.9, fallbackCrossViaUSD("CNY", "USD", 6.9), 1e-9)
	require.InDelta(t, 1, fallbackCrossViaUSD("USD", "EUR", 6.9), 1e-9)
	require.InDelta(t, 6.9, fallbackCrossViaUSD("EUR", "CNY", 6.9), 1e-9)
	require.Equal(t, 0.0, fallbackCrossViaUSD("JPY", "CNY", 6.9))
}

func TestFXService_GetRate_chainOrderRespected(t *testing.T) {
	// 第一条新鲜可用即生效（含 open.er-api 格式响应）
	ts := fxTestServer(t, map[string]http.HandlerFunc{
		"/first": fxJSON(t, map[string]any{
			"result":                "success",
			"base_code":             "USD",
			"time_last_update_unix": time.Now().Unix(),
			"rates":                 map[string]float64{"USD": 1, "CNY": 6.76},
		}),
		"/second": fxJSON(t, map[string]any{
			"base":  "USD",
			"date":  time.Now().UTC().Format("2006-01-02"),
			"rates": map[string]float64{"USD": 1, "CNY": 6.74},
		}),
	})
	s := newTestFXService()
	rate := s.GetRate(context.Background(), "USD", "CNY",
		[]string{ts.URL + "/first", ts.URL + "/second"}, 6.9)
	require.InDelta(t, 6.76, rate, 1e-9)
}
