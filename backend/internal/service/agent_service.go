package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
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
//   - 生命周期计时器（1h idle 销毁 / 24h 保留 / 72h 硬顶）在 manager 侧执行。
//
// 依赖均为接口（AgentManagerInterface / AgentKeyProvisioner / AgentStore），
// 单测用 mock/fake 覆盖（201/202/200/404/manager 500/archive 流式）。
// ──────────────────────────────────────────────────────────────

// defaultAgentModel 是注入容器的默认模型。容器 base_url 指向云间API网关
// （AGENT_MODEL_BASE_URL），用户专属 key 绑定的分组需包含该模型。
const defaultAgentModel = "deepseek-v4-flash"

// AgentV2State 是 manager /v2/agents 响应中单个实例的状态结构。
type AgentV2State struct {
	UserID          int64  `json:"user_id"`
	Status          string `json:"status"` // none/queued/provisioning/active/retained/over_quota
	Port            int    `json:"port,omitempty"`
	AccessHost      string `json:"access_host,omitempty"`
	AccessPassword  string `json:"access_password,omitempty"`
	IdleDeadline    int64  `json:"idle_deadline,omitempty"`    // unix 秒
	RetainDeadline  int64  `json:"retain_deadline,omitempty"`  // unix 秒
	HardcapDeadline int64  `json:"hardcap_deadline,omitempty"` // unix 秒
	Position        int    `json:"position,omitempty"`         // queued 时的排队位置
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

// AgentConfig 是可配置项（数据保留时长/工作空间配额/内存配额）。
type AgentConfig struct {
	DataRetentionHours int `json:"data_retention_hours"`
	WorkspaceQuotaMB   int `json:"workspace_quota_mb"`
	MemoryMB           int `json:"memory_mb"`
}

// AgentUserConfigResponse 是每用户配置响应（全局 + 覆盖 + 生效值）。
type AgentUserConfigResponse struct {
	Global    AgentConfig         `json:"global"`
	Overrides map[string]int      `json:"overrides"`
	Effective AgentConfig         `json:"effective"`
}

// AgentManagerInterface 抽象 NY agent-manager daemon 的 /v2 API（可 mock）。
type AgentManagerInterface interface {
	// Create 请求创建实例。返回状态码语义：201=已激活 202=已排队 200=已存在（幂等）。
	Create(ctx context.Context, userID int64, apiKey string) (*AgentV2State, int, error)
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
}

// AgentKeyProvisioner 抽象用户专属 API key 的创建/吊销（可 mock）。
type AgentKeyProvisioner interface {
	Create(ctx context.Context, userID int64) (key string, keyID int64, err error)
	Revoke(ctx context.Context, keyID int64) error
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

func (c *httpAgentManagerClient) Create(ctx context.Context, userID int64, apiKey string) (*AgentV2State, int, error) {
	resp, err := c.do(ctx, http.MethodPost, "/v2/agents", map[string]any{
		"user_id": userID,
		"api_key": apiKey,
	})
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

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
	defer resp.Body.Close()
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
	defer resp.Body.Close()
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
	defer resp.Body.Close()
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
	defer resp.Body.Close()
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
	defer resp.Body.Close()
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
		resp.Body.Close()
		return nil, errors.New("agent archive not found")
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, fmt.Errorf("agent-manager archive: http %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return resp.Body, nil // 流式返回，调用方负责关闭
}

// apiKeyAgentProvisioner 基于 APIKeyService 的真实 key 创建/吊销。
// 创建时把 key 绑定到用户第一个可用分组（保证可调度，#32 教训：未分组账号不可调度）。
type apiKeyAgentProvisioner struct {
	keyService *APIKeyService
}

func NewAPIKeyAgentProvisioner(keyService *APIKeyService) AgentKeyProvisioner {
	return &apiKeyAgentProvisioner{keyService: keyService}
}

func (p *apiKeyAgentProvisioner) Create(ctx context.Context, userID int64) (string, int64, error) {
	groups, err := p.keyService.GetAvailableGroups(ctx, userID)
	if err != nil {
		return "", 0, fmt.Errorf("agent key: get available groups: %w", err)
	}
	req := CreateAPIKeyRequest{Name: "Agent 服务"}
	if len(groups) > 0 {
		req.GroupID = &groups[0].ID
	}
	key, err := p.keyService.Create(ctx, userID, req)
	if err != nil {
		return "", 0, fmt.Errorf("agent key: create: %w", err)
	}
	return key.Key, key.ID, nil
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
	cfg       *config.Config
}

func NewAgentService(
	store AgentStore,
	manager AgentManagerInterface,
	provision AgentKeyProvisioner,
	cfg *config.Config,
) *AgentService {
	return &AgentService{
		store:     store,
		manager:   manager,
		provision: provision,
		cfg:       cfg,
	}
}

// AgentState 是返回给前端的实例状态。
type AgentState struct {
	Status          string `json:"status"` // not_started / running / queued / provisioning / retained / over_quota / error
	Port            int    `json:"port,omitempty"`
	AccessHost      string `json:"access_host,omitempty"`
	AccessPassword  string `json:"access_password,omitempty"`
	AgentURL        string `json:"agent_url,omitempty"`
	IdleDeadline    int64  `json:"idle_deadline,omitempty"`
	RetainDeadline  int64  `json:"retain_deadline,omitempty"`
	HardcapDeadline int64  `json:"hardcap_deadline,omitempty"`
	Position        int    `json:"position,omitempty"`
	CreatedAt       string `json:"created_at,omitempty"`
}

func (s *AgentService) IsConfigured() bool {
	return s.cfg.Agent.IsConfigured()
}

// StartAgent 启动用户 Agent 实例（薄代理到 manager Create）。
// 201/200 -> running，202 -> queued。manager 错误透传给调用方。
func (s *AgentService) StartAgent(ctx context.Context, userID int64) (*AgentState, error) {
	if !s.cfg.Agent.IsConfigured() {
		return nil, errors.New("agent service not configured (AGENT_MANAGER_URL / AGENT_MANAGER_TOKEN)")
	}

	// 已有记录且非 error/orphaned：幂等返回现状态（不重复创建 key）
	existing, err := s.store.GetByUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	if existing != nil && existing.Status != "error" && existing.Status != "orphaned" {
		return s.mapRowToState(existing), nil
	}

	// 生成用户专属 key（绑定第一个可用分组）
	key, keyID, err := s.provision.Create(ctx, userID)
	if err != nil {
		return nil, err
	}

	// 代理到 manager
	st, code, err := s.manager.Create(ctx, userID, key)
	if err != nil {
		// 容器创建失败：吊销刚创建的 key，不留孤儿
		_ = s.provision.Revoke(ctx, keyID)
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
	}
	if st != nil {
		agent.Port = st.Port
	}
	if err := s.store.Upsert(ctx, agent); err != nil {
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
		state.AgentURL = s.agentURL(st)
	}
	return state, nil
}

// StopAgent 停止并销毁用户 Agent 实例（薄代理到 manager Delete，幂等）。
// manager 报错 -> 保留行状态 error 并返回错误（v1 orphaned 语义由 manager 接管）。
func (s *AgentService) StopAgent(ctx context.Context, userID int64) error {
	existing, err := s.store.GetByUser(ctx, userID)
	if err != nil {
		return err
	}
	if existing == nil {
		return nil // 幂等：无实例 = 成功
	}

	if err := s.manager.Delete(ctx, userID); err != nil {
		existing.Status = "error"
		_ = s.store.Upsert(ctx, existing)
		return fmt.Errorf("stop agent: destroy failed (row preserved as error): %w", err)
	}

	// 销毁成功：吊销 key + 清库
	if err := s.provision.Revoke(ctx, existing.AgentKeyID); err != nil {
		existing.Status = "error"
		_ = s.store.Upsert(ctx, existing)
		return fmt.Errorf("stop agent: revoke key failed (container destroyed): %w", err)
	}
	if err := s.store.DeleteByUser(ctx, userID); err != nil {
		return fmt.Errorf("stop agent: clear: %w", err)
	}
	return nil
}

// GetAgentStatus 返回用户实例状态（薄代理 manager Get；无实例 -> not_started）。
func (s *AgentService) GetAgentStatus(ctx context.Context, userID int64) (*AgentState, error) {
	existing, err := s.store.GetByUser(ctx, userID)
	if err != nil {
		return nil, err
	}

	st, err := s.manager.Get(ctx, userID)
	if err != nil {
		// manager 不可达：回退到库内记录（不致命）
		if existing == nil {
			return &AgentState{Status: "not_started"}, nil
		}
		return s.mapRowToState(existing), nil
	}
	if st == nil {
		// manager 无记录；若本地有记录则反映（可能 manager 已重置）
		if existing == nil {
			return &AgentState{Status: "not_started"}, nil
		}
		return s.mapRowToState(existing), nil
	}

	state := &AgentState{
		Status:          st.Status,
		Port:            st.Port,
		AccessHost:      st.AccessHost,
		AccessPassword:  st.AccessPassword,
		IdleDeadline:    st.IdleDeadline,
		RetainDeadline:  st.RetainDeadline,
		HardcapDeadline: st.HardcapDeadline,
		Position:        st.Position,
	}
	state.AgentURL = s.agentURL(st)
	return state, nil
}

// ListAgents 返回全量实例 + 池统计（管理端）。
func (s *AgentService) ListAgents(ctx context.Context) (*AgentListResponse, error) {
	return s.manager.List(ctx)
}

// DownloadArchive 流式返回用户实例归档（管理端）。
func (s *AgentService) DownloadArchive(ctx context.Context, userID int64) (io.Reader, error) {
	return s.manager.Archive(ctx, userID)
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
