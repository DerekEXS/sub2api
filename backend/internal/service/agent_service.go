package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
)

// ──────────────────────────────────────────────────────────────
// AgentService v2 - 用户「Agent 服务」生命周期管理（薄代理架构）
//
// NY agent-manager 是状态唯一权威源；本服务只做代理 + 计费 key 注入 + 权限。
//   - StartAgent: 调 manager Create（带用户专属 key）-> 201/200 表示 active，202 表示 queued
//   - GetAgentStatus: 代理 manager Get，无记录返回 not_started
//   - StopAgent: 代理 manager Delete；manager 报错 -> 保留行状态 error 并返回错误
//   - ListAgents / DownloadArchive: 管理端代理到 manager
//
// 相比 v1（本地 agents 表 + POST /v1/create）的差异：
//   - v1 的 orphaned 语义（destroy 失败保留 DB 记录待重试）由 manager 侧接管，
//     本服务仅把 manager 的错误透传给调用方，并保留行状态 error 供审计。
//   - 生命周期计时器（idle 销毁 / retain 保留期 / hardcap 硬顶）在 manager 侧执行。
//
// 依赖均为接口（AgentManagerInterface / AgentKeyProvisioner / AgentStore），
// 单测用 mock/fake 覆盖（201/202/200/404/manager 500/archive 流式）。
// ──────────────────────────────────────────────────────────────

// managerCreateTimeout 是 manager Create 的独立超时：容器创建（docker run +
// launcher 初始化）是同步慢操作，需大于前端 axios 30s 超时，让服务端在客户端
// 断开后仍能完成创建并把状态落库（#320 Network error 根因修复）。
const managerCreateTimeout = 150 * time.Second

// AgentV2State 是 manager /v2/agents 响应中单个实例的状态结构。
type AgentV2State struct {
	UserID             int64  `json:"user_id"`
	Status             string `json:"status"` // none/queued/provisioning/active/retained/over_quota
	Port               int    `json:"port,omitempty"`
	AccessHost         string `json:"access_host,omitempty"`
	AccessPassword     string `json:"access_password,omitempty"`
	IdleDeadline       int64  `json:"idle_deadline,omitempty"`        // unix 秒
	RetainDeadline     int64  `json:"retain_deadline,omitempty"`      // unix 秒
	HardcapDeadline    int64  `json:"hardcap_deadline,omitempty"`     // unix 秒
	Position           int    `json:"position,omitempty"`             // queued 时的排队位置
	IdleTimeoutMinutes int    `json:"idle_timeout_minutes,omitempty"` // 前端动态文案（分钟）
	DataRetentionHours int    `json:"data_retention_hours,omitempty"` // 前端动态文案（小时）
	RetainHours        int    `json:"retain_hours,omitempty"`         // 前端动态文案（小时，保留期）#issue2/3
	HardcapHours       int    `json:"hardcap_hours,omitempty"`        // 前端动态文案（小时，硬顶）#issue2/3
}

// AgentPoolStats 是 manager 池统计。
type AgentPoolStats struct {
	FreeGB   int `json:"free_gb"`
	Active   int `json:"active"`
	Queued   int `json:"queued"`
	Archived int `json:"archived"`
}

// AgentListResponse 是 manager GET /v2/agents 的完整响应。
type AgentListResponse struct {
	Agents []AgentV2State `json:"agents"`
	Pool   AgentPoolStats `json:"pool"`
}

// AgentAdminListItem 是管理端实例列表条目（manager 状态 + 用户身份信息，#328）。
type AgentAdminListItem struct {
	AgentV2State
	Email    string `json:"email,omitempty"`
	Username string `json:"username,omitempty"`
}

// AgentAdminListResponse 是管理端实例列表响应（含邮箱/用户名列）。
type AgentAdminListResponse struct {
	Agents []AgentAdminListItem `json:"agents"`
	Pool   AgentPoolStats       `json:"pool"`
}

// AgentConfig 是可配置项（保留期/硬顶/工作空间配额/内存配额/idle 超时）。
// 双轨语义（主人 2026-08-16 规范）：
//   - RetainHours 保留期：每次启动刷新重计时（manager last_started_at），关停后归档保留时长
//   - HardcapHours 硬顶：从首次激活（first_activated_at）起算，永不清零，到期强制销毁+清数据
//
// 旧键 data_retention_hours 由 manager 侧读入时映射为 hardcap_hours（向后兼容）。
type AgentConfig struct {
	RetainHours        int `json:"retain_hours"`
	HardcapHours       int `json:"hardcap_hours"`
	WorkspaceQuotaMB   int `json:"workspace_quota_mb"`
	MemoryMB           int `json:"memory_mb"`
	IdleTimeoutMinutes int `json:"idle_timeout_minutes"`
}

// AgentUserConfigResponse 是每用户配置响应（全局 + 覆盖 + 生效值）。
type AgentUserConfigResponse struct {
	Global    AgentConfig    `json:"global"`
	Overrides map[string]int `json:"overrides"`
	Effective AgentConfig    `json:"effective"`
}

// AgentManagerInterface 抽象 NY agent-manager daemon 的 /v2 API（可 mock）。
type AgentManagerInterface interface {
	// Create 请求创建实例。models 为该用户选定分组支持的模型名列表（可为空，
	// manager 注入容器 config.json 供实例只允许调用这些模型）。
	// 返回状态码语义：201=已激活 202=已排队 200=已存在（幂等）。
	Create(ctx context.Context, userID int64, apiKey string, models []string) (*AgentV2State, int, error)
	// Get 查询单实例；无实例时返回 (nil, nil, nil)。
	Get(ctx context.Context, userID int64) (*AgentV2State, error)
	// Delete 销毁并归档实例（幂等：无实例也返回成功）。
	Delete(ctx context.Context, userID int64) error
	// List 返回全量实例 + 池统计。
	List(ctx context.Context) (*AgentListResponse, error)
	// Archive 流式返回实例归档 tar.gz（io.Reader 透传）。
	Archive(ctx context.Context, userID int64) (io.Reader, error)
	// GetConfig 返回全局配置。
	GetConfig(ctx context.Context) (*AgentConfig, error)
	// UpdateConfig 更新全局配置（只更新传入的非零字段）。
	UpdateConfig(ctx context.Context, cfg AgentConfig) (*AgentConfig, error)
	// GetUserConfig 返回每用户配置（全局 + 覆盖 + 生效值）。
	GetUserConfig(ctx context.Context, userID int64) (*AgentUserConfigResponse, error)
	// UpdateUserConfig 更新每用户覆盖（值为 0 表示清除覆盖）。
	UpdateUserConfig(ctx context.Context, userID int64, overrides map[string]int) (*AgentUserConfigResponse, error)
	// Metrics 返回 Agent 后端宿主硬件指标（CPU/内存/磁盘/负载/容器数，#328 仪表盘）。
	Metrics(ctx context.Context) (map[string]any, error)
}

// AgentKeyProvisioner 抽象用户专属 API key 的创建/吊销（可 mock）。
type AgentKeyProvisioner interface {
	// Create 创建用户专属 key 并返回其绑定分组支持的模型名列表（供 manager 注入实例）。
	Create(ctx context.Context, userID int64) (key string, keyID int64, models []string, err error)
	Revoke(ctx context.Context, keyID int64) error
	// CleanupOrphans 删除该用户名下所有名为 "Agent" 的历史遗留 key（best-effort，
	// StartAgent 重建前调用，防止 stop 半途失败/旧版本泄漏累积孤儿 key，#328）。
	CleanupOrphans(ctx context.Context, userID int64)
}

// Agent 是 agents 表的行（记录后端已知的实例映射，用于 key 生命周期管理）。
//
// AgentKey 存储明文 API key（设计如此）：该 key 仅用于注入用户实例的 config.json，
// destroy 时立即吊销（软删 deleted_at）。key 不出现在日志/备份/错误响应中。
type Agent struct {
	ID            int64
	UserID        int64
	ContainerName string
	Port          int
	Status        string // running / queued / stopped / orphaned / error
	AgentKey      string // 明文存储，destroy 即吊销（见上方注释）
	AgentKeyID    int64
	CreatedAt     time.Time
	LastActiveAt  time.Time
}

// AgentStore 抽象 agents 表的读写（可 mock）。
type AgentStore interface {
	GetByUser(ctx context.Context, userID int64) (*Agent, error)
	Upsert(ctx context.Context, a *Agent) error
	DeleteByUser(ctx context.Context, userID int64) error
}

// ──────────────────────────────────────────────────────────────
// 真实实现
// ──────────────────────────────────────────────────────────────

// sqlAgentStore 基于 database/sql 的 agents 表读写。
type sqlAgentStore struct {
	db *sql.DB
}

func NewSQLAgentStore(db *sql.DB) AgentStore {
	return &sqlAgentStore{db: db}
}

func (s *sqlAgentStore) GetByUser(ctx context.Context, userID int64) (*Agent, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, user_id, container_name, port, status, agent_key, agent_key_id, created_at, last_active_at
		FROM agents WHERE user_id = $1`, userID)
	var a Agent
	err := row.Scan(&a.ID, &a.UserID, &a.ContainerName, &a.Port, &a.Status,
		&a.AgentKey, &a.AgentKeyID, &a.CreatedAt, &a.LastActiveAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("agent store get by user: %w", err)
	}
	return &a, nil
}

func (s *sqlAgentStore) Upsert(ctx context.Context, a *Agent) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO agents (user_id, container_name, port, status, agent_key, agent_key_id, created_at, last_active_at)
		VALUES ($1, $2, $3, $4, $5, $6, NOW(), NOW())
		ON CONFLICT (user_id) DO UPDATE SET
			container_name = EXCLUDED.container_name,
			port = EXCLUDED.port,
			status = EXCLUDED.status,
			agent_key = EXCLUDED.agent_key,
			agent_key_id = EXCLUDED.agent_key_id,
			last_active_at = NOW()`,
		a.UserID, a.ContainerName, a.Port, a.Status, a.AgentKey, a.AgentKeyID)
	if err != nil {
		return fmt.Errorf("agent store upsert: %w", err)
	}
	return nil
}

func (s *sqlAgentStore) DeleteByUser(ctx context.Context, userID int64) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM agents WHERE user_id = $1`, userID); err != nil {
		return fmt.Errorf("agent store delete: %w", err)
	}
	return nil
}

// httpAgentManagerClient 调用 NY agent-manager daemon（127.0.0.1:9180，X-Agent-Token 认证）。
type httpAgentManagerClient struct {
	baseURL string
	token   string
	client  *http.Client
}

func NewHTTPAgentManagerClient(baseURL, token string) AgentManagerInterface {
	return &httpAgentManagerClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		client:  &http.Client{Timeout: 120 * time.Second}, // create 需等待容器 healthy（~30s），archive 需留流式读
	}
}

func (c *httpAgentManagerClient) do(ctx context.Context, method, path string, payload any) (*http.Response, error) {
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("marshal request: %w", err)
		}
		body = strings.NewReader(string(raw))
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Agent-Token", c.token)
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("agent-manager %s %s: %w", method, path, err)
	}
	return resp, nil
}

func (c *httpAgentManagerClient) Create(ctx context.Context, userID int64, apiKey string, models []string) (*AgentV2State, int, error) {
	payload := map[string]any{
		"user_id": userID,
		"api_key": apiKey,
	}
	if len(models) > 0 {
		payload["models"] = models
	}
	resp, err := c.do(ctx, http.MethodPost, "/v2/agents", payload)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()

	// archive 之外的 /v2 API 全部返回 JSON
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("read response: %w", err)
	}

	switch resp.StatusCode {
	case http.StatusCreated, http.StatusOK, http.StatusAccepted:
		var st AgentV2State
		if err := json.Unmarshal(raw, &st); err != nil {
			return nil, resp.StatusCode, fmt.Errorf("parse response: %w", err)
		}
		return &st, resp.StatusCode, nil
	default:
		return nil, resp.StatusCode, fmt.Errorf("agent-manager create: http %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
}

func (c *httpAgentManagerClient) Get(ctx context.Context, userID int64) (*AgentV2State, error) {
	resp, err := c.do(ctx, http.MethodGet, "/v2/agents/"+strconv.FormatInt(userID, 10), nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil // 无实例
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("agent-manager get: http %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var st AgentV2State
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	return &st, nil
}

func (c *httpAgentManagerClient) Delete(ctx context.Context, userID int64) error {
	resp, err := c.do(ctx, http.MethodDelete, "/v2/agents/"+strconv.FormatInt(userID, 10), nil)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return nil // 幂等：无实例 = 成功
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("agent-manager delete: http %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return nil
}

func (c *httpAgentManagerClient) List(ctx context.Context) (*AgentListResponse, error) {
	resp, err := c.do(ctx, http.MethodGet, "/v2/agents", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("agent-manager list: http %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out AgentListResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	return &out, nil
}

func (c *httpAgentManagerClient) Metrics(ctx context.Context) (map[string]any, error) {
	raw, err := c.doJSONGet(ctx, "/v2/metrics")
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("parse metrics: %w", err)
	}
	return out, nil
}

func (c *httpAgentManagerClient) GetConfig(ctx context.Context) (*AgentConfig, error) {
	raw, err := c.doJSONGet(ctx, "/v2/config")
	if err != nil {
		return nil, err
	}
	var cfg AgentConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	return &cfg, nil
}

func (c *httpAgentManagerClient) UpdateConfig(ctx context.Context, cfg AgentConfig) (*AgentConfig, error) {
	raw, err := c.doJSONReq(ctx, http.MethodPut, "/v2/config", cfg)
	if err != nil {
		return nil, err
	}
	var out AgentConfig
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	return &out, nil
}

func (c *httpAgentManagerClient) GetUserConfig(ctx context.Context, userID int64) (*AgentUserConfigResponse, error) {
	raw, err := c.doJSONGet(ctx, "/v2/agents/"+strconv.FormatInt(userID, 10)+"/config")
	if err != nil {
		return nil, err
	}
	var out AgentUserConfigResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("parse user config: %w", err)
	}
	return &out, nil
}

func (c *httpAgentManagerClient) UpdateUserConfig(ctx context.Context, userID int64, overrides map[string]int) (*AgentUserConfigResponse, error) {
	raw, err := c.doJSONReq(ctx, http.MethodPut, "/v2/agents/"+strconv.FormatInt(userID, 10)+"/config", overrides)
	if err != nil {
		return nil, err
	}
	var out AgentUserConfigResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("parse user config: %w", err)
	}
	return &out, nil
}

// doJSONGet GET 一个 /v2 JSON 端点并返回原始 JSON 字节（状态码必须 200）。
func (c *httpAgentManagerClient) doJSONGet(ctx context.Context, path string) ([]byte, error) {
	resp, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("agent-manager get %s: http %d: %s", path, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return raw, nil
}

// doJSONReq 发送 JSON 请求并返回原始 JSON 字节（状态码必须 200）。
func (c *httpAgentManagerClient) doJSONReq(ctx context.Context, method, path string, payload any) ([]byte, error) {
	resp, err := c.do(ctx, method, path, payload)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("agent-manager %s %s: http %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return raw, nil
}

func (c *httpAgentManagerClient) Archive(ctx context.Context, userID int64) (io.Reader, error) {
	resp, err := c.do(ctx, http.MethodGet, "/v2/agents/"+strconv.FormatInt(userID, 10)+"/archive", nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		_ = resp.Body.Close()
		return nil, errors.New("agent archive not found")
	}
	if resp.StatusCode != http.StatusOK {
		defer func() { _ = resp.Body.Close() }()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, fmt.Errorf("agent-manager archive: http %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return resp.Body, nil // 流式返回，调用方负责关闭
}

// apiKeyAgentProvisioner 基于 APIKeyService 的真实 key 创建/吊销。
// 创建时把 key 绑定到用户「视角倍率最低」的可用分组（保证可调度，#32 教训：未分组账号不可调度；
// 优先 openai 平台，无 openai 分组则选全量倍率最低者），并把该分组支持的模型名列表
// 随 Create 请求透传给 manager（注入实例 config.json，限制实例可调用模型）。
type apiKeyAgentProvisioner struct {
	keyService        *APIKeyService
	modelPlazaService *ModelPlazaService
}

func NewAPIKeyAgentProvisioner(keyService *APIKeyService, modelPlazaService *ModelPlazaService) AgentKeyProvisioner {
	return &apiKeyAgentProvisioner{keyService: keyService, modelPlazaService: modelPlazaService}
}

func (p *apiKeyAgentProvisioner) Create(ctx context.Context, userID int64) (string, int64, []string, error) {
	groups, err := p.keyService.GetAvailableGroups(ctx, userID)
	if err != nil {
		return "", 0, nil, fmt.Errorf("agent key: get available groups: %w", err)
	}
	group, ok := pickAgentGroup(groups)
	var models []string
	req := CreateAPIKeyRequest{Name: "Agent"}
	if ok {
		req.GroupID = &group.ID
		models = p.groupModels(ctx, &group)
	}
	key, err := p.keyService.Create(ctx, userID, req)
	if err != nil {
		return "", 0, nil, fmt.Errorf("agent key: create: %w", err)
	}
	return key.Key, key.ID, models, nil
}

// pickAgentGroup 从用户可用分组中选择绑定目标：优先 platform==openai 的分组，
// 组内按 rate_multiplier 升序取第一个（= 用户视角倍率最低）；无 openai 分组时
// 在全部分组中任选倍率最低者。返回 (group, true)；空列表返回 (Group{}, false)。
func pickAgentGroup(groups []Group) (Group, bool) {
	if len(groups) == 0 {
		return Group{}, false
	}
	sort.SliceStable(groups, func(i, j int) bool {
		if groups[i].Platform == PlatformOpenAI && groups[j].Platform != PlatformOpenAI {
			return true
		}
		if groups[i].Platform != PlatformOpenAI && groups[j].Platform == PlatformOpenAI {
			return false
		}
		if groups[i].RateMultiplier != groups[j].RateMultiplier {
			return groups[i].RateMultiplier < groups[j].RateMultiplier
		}
		return groups[i].ID < groups[j].ID
	})
	return groups[0], true
}

// groupModels 返回选定分组支持的模型名列表：
//   - 分组显式配置了 models_list_config（CustomModelsListEnabled）时直接用其列表；
//   - 否则走 ModelPlazaService.ListGroups 的渠道 model_mapping/SupportedModels 口径。
func (p *apiKeyAgentProvisioner) groupModels(ctx context.Context, group *Group) []string {
	if group.CustomModelsListEnabled() {
		return append([]string(nil), group.ModelsListConfig.Models...)
	}
	if p.modelPlazaService == nil {
		return nil
	}
	plaza, err := p.modelPlazaService.ListGroups(ctx)
	if err != nil {
		return nil // 模型列表是增强信息，拿不到不阻塞 key 创建
	}
	for i := range plaza {
		if plaza[i].ID != group.ID {
			continue
		}
		names := make([]string, 0, len(plaza[i].Models))
		for _, m := range plaza[i].Models {
			names = append(names, m.Name)
		}
		return names
	}
	return nil
}

// agentKeyName 是 Agent 服务自动生成 key 的固定名称（清扫孤儿 key 按此名匹配）。
const agentKeyName = "Agent"

// CleanupOrphans 删除该用户名下所有名为 "Agent" 的 key（best-effort，失败仅跳过）。
// 分页扫描用户 key 列表（每页 100，最多 10 页兜底），逐个按名删除。
func (p *apiKeyAgentProvisioner) CleanupOrphans(ctx context.Context, userID int64) {
	for page := 1; page <= 10; page++ {
		keys, res, err := p.keyService.List(ctx, userID,
			pagination.PaginationParams{Page: page, PageSize: 100}, APIKeyListFilters{})
		if err != nil {
			return
		}
		for i := range keys {
			if keys[i].Name == agentKeyName {
				_ = p.keyService.Delete(ctx, keys[i].ID, userID)
			}
		}
		if res == nil || int64(page*100) >= res.Total {
			return
		}
	}
}

func (p *apiKeyAgentProvisioner) Revoke(ctx context.Context, keyID int64) error {
	if keyID <= 0 {
		return nil
	}
	// keyID 归属校验在 APIKeyService.Delete 内部完成
	_, ownerID, err := p.keyService.apiKeyRepo.GetKeyAndOwnerID(ctx, keyID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil // 已吊销
		}
		return fmt.Errorf("agent key: lookup: %w", err)
	}
	if err := p.keyService.Delete(ctx, keyID, ownerID); err != nil && !errors.Is(err, ErrInsufficientPerms) {
		return fmt.Errorf("agent key: revoke: %w", err)
	}
	return nil
}

// ──────────────────────────────────────────────────────────────
// AgentService v2
// ──────────────────────────────────────────────────────────────

// AgentService 编排 Agent 实例的完整生命周期（薄代理到 NY manager）。
type AgentService struct {
	store     AgentStore
	manager   AgentManagerInterface
	provision AgentKeyProvisioner
	userRepo  UserRepository // 管理端列表补全邮箱/用户名（#328；可为 nil，nil 时跳过补全）
	cfg       *config.Config
}

func NewAgentService(
	store AgentStore,
	manager AgentManagerInterface,
	provision AgentKeyProvisioner,
	userRepo UserRepository,
	cfg *config.Config,
) *AgentService {
	return &AgentService{
		store:     store,
		manager:   manager,
		provision: provision,
		userRepo:  userRepo,
		cfg:       cfg,
	}
}

// AgentState 是返回给前端的实例状态。
type AgentState struct {
	Status             string `json:"status"` // not_started / running / queued / provisioning / retained / over_quota / error
	Port               int    `json:"port,omitempty"`
	AccessHost         string `json:"access_host,omitempty"`
	AccessPassword     string `json:"access_password,omitempty"`
	AgentURL           string `json:"agent_url,omitempty"`
	IdleDeadline       int64  `json:"idle_deadline,omitempty"`
	RetainDeadline     int64  `json:"retain_deadline,omitempty"`
	HardcapDeadline    int64  `json:"hardcap_deadline,omitempty"`
	Position           int    `json:"position,omitempty"`
	IdleTimeoutMinutes int    `json:"idle_timeout_minutes,omitempty"` // 前端动态文案（分钟）
	DataRetentionHours int    `json:"data_retention_hours,omitempty"` // 前端动态文案（小时）
	RetainHours        int    `json:"retain_hours,omitempty"`         // 前端动态文案（小时，保留期）#issue2/3
	HardcapHours       int    `json:"hardcap_hours,omitempty"`        // 前端动态文案（小时，硬顶）#issue2/3
	CreatedAt          string `json:"created_at,omitempty"`
}

func (s *AgentService) IsConfigured() bool {
	return s.cfg.Agent.IsConfigured()
}

// notStartedWithDefaults 返回 not_started 状态，附带全局 Agent 配置默认值
// （retain_hours/hardcap_hours/idle_timeout_minutes）。#issue2/3：前端在未启动时
// 也能显示与实际配置一致的说明文案（而非硬编码 168h）。
// manager GetConfig 失败时回退到 0（前端有 72/168/30 兜底默认）。
func (s *AgentService) notStartedWithDefaults(ctx context.Context) *AgentState {
	st := &AgentState{Status: "not_started"}
	if cfg, err := s.manager.GetConfig(ctx); err == nil && cfg != nil {
		st.RetainHours = cfg.RetainHours
		st.HardcapHours = cfg.HardcapHours
		st.IdleTimeoutMinutes = cfg.IdleTimeoutMinutes
		st.DataRetentionHours = cfg.RetainHours // 兼容旧字段
	}
	return st
}

// StartAgent 启动用户 Agent 实例（薄代理到 manager Create）。
// 201/200 -> running，202 -> queued。manager 错误透传给调用方。
func (s *AgentService) StartAgent(ctx context.Context, userID int64) (*AgentState, error) {
	if !s.cfg.Agent.IsConfigured() {
		return nil, errors.New("agent service not configured (AGENT_MANAGER_URL / AGENT_MANAGER_TOKEN)")
	}

	// 已有记录且非 error/orphaned：幂等返回现状态（不重复创建 key）。
	// 用 manager 实时状态补全 access_host/password/deadlines（DB 行不含密码，
	// 幂等路径也必须给前端完整的 access_password 等字段，#320 验收）。
	existing, err := s.store.GetByUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	// 幂等短路判定（#325 二修）：不仅 DB 行状态要活跃，manager 实时状态也必须
	// active/queued/provisioning——否则（如 DB 残留 active 行但 manager 实例已被 idle
	// 销毁成 retained）走重建，避免假短路返回无密码旧行、用户看到"无实例"。
	if existing != nil && (existing.Status == "active" || existing.Status == "queued" ||
		existing.Status == "provisioning" || existing.Status == "running") {
		st, gerr := s.manager.Get(ctx, userID)
		live := gerr == nil && st != nil &&
			(st.Status == "active" || st.Status == "queued" || st.Status == "provisioning")
		if live {
			state := s.mapRowToState(existing)
			state.AccessHost = st.AccessHost
			state.AccessPassword = st.AccessPassword
			state.IdleDeadline = st.IdleDeadline
			state.RetainDeadline = st.RetainDeadline
			state.HardcapDeadline = st.HardcapDeadline
			state.Position = st.Position
			state.Port = st.Port
			state.IdleTimeoutMinutes = st.IdleTimeoutMinutes
			state.DataRetentionHours = st.DataRetentionHours
			state.AgentURL = s.agentURL(st)
			return state, nil
		}
		// manager 无活跃实例：吊销残留行的旧 key + 清 DB 行，走下方重建。
		// ⚠️ 不吊销就重建会每次泄漏一个 "Agent" key（#328 实测：测试号积累 11 个孤儿 key）。
		_ = s.provision.Revoke(ctx, existing.AgentKeyID)
		_ = s.store.DeleteByUser(ctx, userID)
		existing = nil
	}
	// 兜底清扫：无论 DB 行状态如何，把该用户名下所有历史遗留的 "Agent" key 清掉
	// （行状态 error / 前次 stop 半途失败 / 旧版本泄漏的都在此收口），再创建新 key。
	s.provision.CleanupOrphans(ctx, userID)

	// 生成用户专属 key（绑定倍率最低的可用分组 + 模型列表）
	key, keyID, models, err := s.provision.Create(ctx, userID)
	if err != nil {
		return nil, err
	}

	// 代理到 manager。⚠️ 关键：不能沿用客户端请求 ctx —— 容器创建（docker run +
	// launcher 初始化）可能长达 60s+，而前端 axios 超时仅 30s；客户端断开会导致
	// gin 取消 ctx，manager 调用被中断（"context canceled"），key 被误吊销而
	// manager 侧容器仍在创建 → 孤儿实例。改用 WithoutCancel + 独立超时，
	// 客户端断开不影响容器创建与落库，状态最终由 /agent/status 对账。
	// （实测：断开后 manager 调用成功但 Upsert 用客户端 ctx 会报
	//  "persist: context canceled" → 行缺失、key 无法在 stop 时吊销，必须一并使用 mgrCtx）
	mgrCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), managerCreateTimeout)
	defer cancel()
	st, code, err := s.manager.Create(mgrCtx, userID, key, models)
	if err != nil {
		// 容器创建真实失败：吊销刚创建的 key，不留孤儿
		_ = s.provision.Revoke(mgrCtx, keyID)
		return nil, fmt.Errorf("start agent: %w", err)
	}

	status := "running"
	if code == http.StatusAccepted {
		status = "queued"
	}
	if st != nil && st.Status != "" {
		status = st.Status
	}

	agent := &Agent{
		UserID:        userID,
		ContainerName: fmt.Sprintf("agent-%d", userID),
		Port:          0,
		Status:        status,
		AgentKey:      key,
		AgentKeyID:    keyID,
		CreatedAt:     time.Now(),
	}
	if st != nil {
		agent.Port = st.Port
	}
	if err := s.store.Upsert(mgrCtx, agent); err != nil {
		return nil, fmt.Errorf("start agent: persist: %w", err)
	}

	state := s.mapRowToState(agent)
	if st != nil {
		state.AccessHost = st.AccessHost
		state.AccessPassword = st.AccessPassword
		state.IdleDeadline = st.IdleDeadline
		state.RetainDeadline = st.RetainDeadline
		state.HardcapDeadline = st.HardcapDeadline
		state.Position = st.Position
		state.Port = st.Port
		state.IdleTimeoutMinutes = st.IdleTimeoutMinutes
		state.DataRetentionHours = st.DataRetentionHours
		state.AgentURL = s.agentURL(st)
	}
	return state, nil
}

// StopAgent 停止并销毁用户 Agent 实例（薄代理到 manager Delete，幂等）。
// manager 报错 -> 保留行状态 error 并返回错误（v1 orphaned 语义由 manager 接管）。
//
// ⚠️ 无论本地 agents 行是否存在都调用 manager.Delete（manager 端幂等）：
// StartAgent 若因客户端断开/超时中断可能没有本地行但 manager 实例已创建，
// 只按本地行短路会让孤儿实例永远销毁不掉（#320 实测：stop 变 no-op）。
func (s *AgentService) StopAgent(ctx context.Context, userID int64) error {
	existing, err := s.store.GetByUser(ctx, userID)
	if err != nil {
		return err
	}

	if err := s.manager.Delete(ctx, userID); err != nil {
		if existing != nil {
			existing.Status = "error"
			_ = s.store.Upsert(ctx, existing)
		}
		return fmt.Errorf("stop agent: destroy failed (row preserved as error): %w", err)
	}

	// 销毁成功：吊销 key + 清库（无本地行则无需吊销）
	if existing != nil {
		if err := s.provision.Revoke(ctx, existing.AgentKeyID); err != nil {
			existing.Status = "error"
			_ = s.store.Upsert(ctx, existing)
			return fmt.Errorf("stop agent: revoke key failed (container destroyed): %w", err)
		}
	}
	if existing != nil {
		if err := s.store.DeleteByUser(ctx, userID); err != nil {
			return fmt.Errorf("stop agent: clear: %w", err)
		}
	}
	return nil
}

// GetAgentStatus 返回用户实例状态（薄代理 manager Get；无实例 -> not_started）。
// #issue2/3：not_started 时也返回全局配置默认值（retain_hours/hardcap_hours/idle_timeout_minutes），
// 让前端说明文案与实际配置一致（否则前端永远显示硬编码默认 168h）。
func (s *AgentService) GetAgentStatus(ctx context.Context, userID int64) (*AgentState, error) {
	existing, err := s.store.GetByUser(ctx, userID)
	if err != nil {
		return nil, err
	}

	st, err := s.manager.Get(ctx, userID)
	if err != nil {
		// manager 不可达：回退到库内记录（不致命）
		if existing == nil {
			return s.notStartedWithDefaults(ctx), nil
		}
		return s.mapRowToState(existing), nil
	}
	if st == nil {
		// manager 无记录；若本地有记录则反映（可能 manager 已重置）
		if existing == nil {
			return s.notStartedWithDefaults(ctx), nil
		}
		return s.mapRowToState(existing), nil
	}

	state := &AgentState{
		Status:             st.Status,
		Port:               st.Port,
		AccessHost:         st.AccessHost,
		AccessPassword:     st.AccessPassword,
		IdleDeadline:       st.IdleDeadline,
		RetainDeadline:     st.RetainDeadline,
		HardcapDeadline:    st.HardcapDeadline,
		Position:           st.Position,
		IdleTimeoutMinutes: st.IdleTimeoutMinutes,
		DataRetentionHours: st.DataRetentionHours,
		RetainHours:        st.RetainHours,
		HardcapHours:       st.HardcapHours,
	}
	state.AgentURL = s.agentURL(st)
	return state, nil
}

// ListAgents 返回全量实例 + 池统计（管理端），每条补全用户邮箱/用户名（#328）。
// 用户查询失败不阻塞列表（身份列留空）。
func (s *AgentService) ListAgents(ctx context.Context) (*AgentAdminListResponse, error) {
	lst, err := s.manager.List(ctx)
	if err != nil {
		return nil, err
	}
	out := &AgentAdminListResponse{Pool: lst.Pool, Agents: make([]AgentAdminListItem, 0, len(lst.Agents))}
	for _, a := range lst.Agents {
		item := AgentAdminListItem{AgentV2State: a}
		if s.userRepo != nil && a.UserID > 0 {
			if u, uerr := s.userRepo.GetByIDIncludeDeleted(ctx, a.UserID); uerr == nil && u != nil {
				item.Email = u.Email
				item.Username = u.Username
			}
		}
		out.Agents = append(out.Agents, item)
	}
	return out, nil
}

// DownloadArchive 流式返回用户实例归档（管理端）。
func (s *AgentService) DownloadArchive(ctx context.Context, userID int64) (io.Reader, error) {
	return s.manager.Archive(ctx, userID)
}

// GetAgentMetrics 返回 Agent 后端宿主硬件指标（管理端仪表盘，#328）。
func (s *AgentService) GetAgentMetrics(ctx context.Context) (map[string]any, error) {
	return s.manager.Metrics(ctx)
}

// LookupUserEmail 按 ID 查用户邮箱（含已软删；查不到返回空串）。
// 供管理端审计列表补全身份列（#328），失败不报错。
func (s *AgentService) LookupUserEmail(ctx context.Context, userID int64) string {
	if s.userRepo == nil || userID <= 0 {
		return ""
	}
	u, err := s.userRepo.GetByIDIncludeDeleted(ctx, userID)
	if err != nil || u == nil {
		return ""
	}
	return u.Email
}

// GetAgentConfig 返回全局 Agent 配置（管理端）。
func (s *AgentService) GetAgentConfig(ctx context.Context) (*AgentConfig, error) {
	return s.manager.GetConfig(ctx)
}

// UpdateAgentConfig 更新全局 Agent 配置（管理端）。
func (s *AgentService) UpdateAgentConfig(ctx context.Context, cfg AgentConfig) (*AgentConfig, error) {
	return s.manager.UpdateConfig(ctx, cfg)
}

// GetAgentUserConfig 返回每用户 Agent 配置（管理端）。
func (s *AgentService) GetAgentUserConfig(ctx context.Context, userID int64) (*AgentUserConfigResponse, error) {
	return s.manager.GetUserConfig(ctx, userID)
}

// UpdateAgentUserConfig 更新每用户 Agent 配置覆盖（管理端；0 = 清除覆盖）。
func (s *AgentService) UpdateAgentUserConfig(ctx context.Context, userID int64, overrides map[string]int) (*AgentUserConfigResponse, error) {
	return s.manager.UpdateUserConfig(ctx, userID, overrides)
}

// mapRowToState 从库内记录构建状态（manager 不可达时回退用）。
func (s *AgentService) mapRowToState(a *Agent) *AgentState {
	st := &AgentState{
		Status:    a.Status,
		Port:      a.Port,
		CreatedAt: a.CreatedAt.Format(time.RFC3339),
	}
	if (a.Status == "running" || a.Status == "active") && a.Port > 0 {
		st.AgentURL = s.agentURL(&AgentV2State{Port: a.Port})
	}
	return st
}

func (s *AgentService) agentURL(st *AgentV2State) string {
	if st == nil || st.Port <= 0 {
		return ""
	}
	base := s.cfg.Agent.PublicURLBase
	if base == "" {
		return ""
	}
	return fmt.Sprintf("%s:%d", strings.TrimRight(base, "/"), st.Port)
}
