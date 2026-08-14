package service

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

// ──────────────────────────────────────────────────────────────
// AgentService - 用户「Agent 服务」生命周期管理
//
// 每个用户最多一个实例（agents 表 UNIQUE(user_id)）：
//   - StartAgent: 创建用户专属 API key（绑用户第一个可用分组）-> 调 NY agent-manager
//     POST /v1/create（严格隔离容器）-> 写 agents 表。已有实例则返回现状态（幂等）。
//     若 DB 中存在 orphaned 记录（上次 stop 失败遗留），先清理旧容器再创建新实例。
//   - StopAgent: 调 agent-manager POST /v1/destroy（容器销毁、数据目录保留 7 天）
//     -> 吊销用户专属 key -> 清 agents 表。
//     destroy 失败时保留 DB 记录（status=orphaned）+ 不吊销 key，等下次重试。
//   - GetAgentStatus: 读 agents 表 + agent-manager GET /v1/status 合并返回。
//
// 依赖均为接口（AgentManagerClient / AgentKeyProvisioner / AgentStore），
// 单测用 mock/fake 覆盖生命周期（start/stop/status/重复 start 幂等/stop 失败保留记录）。
// ──────────────────────────────────────────────────────────────

// defaultAgentModel 是注入容器的默认模型。容器 base_url 指向云间API网关
// （AGENT_MODEL_BASE_URL），用户专属 key 绑定的分组需包含该模型。
const defaultAgentModel = "deepseek-v4-flash"

// AgentManagerStatus 是 agent-manager /v1/status 的响应结构。
type AgentManagerStatus struct {
	Status string `json:"status"` // running / stopped / missing
	Port   int    `json:"port"`
}

// AgentManagerClient 抽象 NY agent-manager daemon 的 HTTP 接口（可 mock）。
type AgentManagerClient interface {
	Create(ctx context.Context, name, apiKey, baseURL, model string) (int, error)
	Destroy(ctx context.Context, name string) error
	Status(ctx context.Context, name string) (AgentManagerStatus, error)
}

// AgentKeyProvisioner 抽象用户专属 API key 的创建/吊销（可 mock）。
type AgentKeyProvisioner interface {
	Create(ctx context.Context, userID int64) (key string, keyID int64, err error)
	Revoke(ctx context.Context, keyID int64) error
}

// Agent 是 agents 表的一行（用户实例记录）。
//
// AgentKey 存储明文 API key（设计如此）：该 key 仅用于注入用户实例的 config.json，
// destroy 时立即吊销（软删 deleted_at）。key 不出现在日志/备份/错误响应中。
type Agent struct {
	ID            int64
	UserID        int64
	ContainerName string
	Port          int
	Status        string // running / stopped / orphaned
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

func NewHTTPAgentManagerClient(baseURL, token string) AgentManagerClient {
	return &httpAgentManagerClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		client:  &http.Client{Timeout: 90 * time.Second}, // create 需等待容器 healthy（~30s）
	}
}

func (c *httpAgentManagerClient) doJSON(ctx context.Context, method, path string, payload any) (map[string]any, error) {
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("marshal request: %w", err)
		}
		body = bytes.NewReader(raw)
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
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("agent-manager %s %s: http %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	if ok, _ := out["ok"].(bool); !ok {
		msg, _ := out["error"].(string)
		return nil, fmt.Errorf("agent-manager %s %s: %s", method, path, msg)
	}
	return out, nil
}

func (c *httpAgentManagerClient) Create(ctx context.Context, name, apiKey, baseURL, model string) (int, error) {
	out, err := c.doJSON(ctx, http.MethodPost, "/v1/create", map[string]any{
		"name":     name,
		"api_key":  apiKey,
		"base_url": baseURL,
		"model":    model,
	})
	if err != nil {
		return 0, err
	}
	port, _ := out["port"].(float64)
	return int(port), nil
}

func (c *httpAgentManagerClient) Destroy(ctx context.Context, name string) error {
	_, err := c.doJSON(ctx, http.MethodPost, "/v1/destroy", map[string]any{"name": name})
	return err
}

func (c *httpAgentManagerClient) Status(ctx context.Context, name string) (AgentManagerStatus, error) {
	out, err := c.doJSON(ctx, http.MethodGet, "/v1/status/"+name, nil)
	if err != nil {
		return AgentManagerStatus{}, err
	}
	st := AgentManagerStatus{}
	st.Status, _ = out["status"].(string)
	if p, ok := out["port"].(float64); ok {
		st.Port = int(p)
	}
	return st, nil
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
// AgentService
// ──────────────────────────────────────────────────────────────

// AgentService 编排 Agent 实例的完整生命周期。
type AgentService struct {
	store      AgentStore
	manager    AgentManagerClient
	provision  AgentKeyProvisioner
	cfg        *config.Config
	httpClient *http.Client
}

func NewAgentService(
	store AgentStore,
	manager AgentManagerClient,
	provision AgentKeyProvisioner,
	cfg *config.Config,
) *AgentService {
	return &AgentService{
		store:      store,
		manager:    manager,
		provision:  provision,
		cfg:        cfg,
		httpClient: &http.Client{Timeout: 90 * time.Second},
	}
}

// AgentState 是返回给前端的实例状态（含 agent_url）。
type AgentState struct {
	Status    string `json:"status"` // not_started / running / stopped / orphaned / error
	Port      int    `json:"port,omitempty"`
	AgentURL  string `json:"agent_url,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
}

func containerNameFor(userID int64) string {
	return fmt.Sprintf("agent-%d", userID)
}

// StartAgent 启动用户 Agent 实例。
//   - 已有 running 实例：返回现状态（幂等，不重复创建 key/容器）
//   - 已有 orphaned 记录（上次 stop 失败遗留）：先清理旧容器+吊销旧 key，再创建新实例
func (s *AgentService) StartAgent(ctx context.Context, userID int64) (*AgentState, error) {
	if !s.cfg.Agent.IsConfigured() {
		return nil, errors.New("agent service not configured (AGENT_MANAGER_URL / AGENT_MANAGER_TOKEN)")
	}

	existing, err := s.store.GetByUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		// orphaned 记录：上次 stop 失败遗留的孤儿容器，先清理再重建（MAJOR 2 对账）
		if existing.Status == "orphaned" {
			_ = s.manager.Destroy(ctx, existing.ContainerName) // best-effort 清理旧容器
			_ = s.provision.Revoke(ctx, existing.AgentKeyID)   // 吊销旧 key
			_ = s.store.DeleteByUser(ctx, userID)              // 清旧记录
			// 继续往下创建新实例
		} else {
			// 幂等：已有实例直接返回现状态（不重复创建 key）
			return s.stateFromRow(ctx, existing)
		}
	}

	name := containerNameFor(userID)
	key, keyID, err := s.provision.Create(ctx, userID)
	if err != nil {
		return nil, err
	}

	model := defaultAgentModel
	baseURL := s.cfg.Agent.ModelBaseURL
	if baseURL == "" {
		baseURL = s.cfg.Server.FrontendURL + "/v1"
	}
	port, err := s.manager.Create(ctx, name, key, baseURL, model)
	if err != nil {
		// 容器创建失败：吊销刚创建的 key，不留孤儿
		_ = s.provision.Revoke(ctx, keyID)
		return nil, fmt.Errorf("start agent: %w", err)
	}

	agent := &Agent{
		UserID:        userID,
		ContainerName: name,
		Port:          port,
		Status:        "running",
		AgentKey:      key,
		AgentKeyID:    keyID,
	}
	if err := s.store.Upsert(ctx, agent); err != nil {
		return nil, fmt.Errorf("start agent: persist: %w", err)
	}
	return &AgentState{
		Status:    "running",
		Port:      port,
		AgentURL:  s.agentURL(port),
		CreatedAt: time.Now().Format(time.RFC3339),
	}, nil
}

// StopAgent 停止并销毁用户 Agent 实例（幂等：无实例时直接返回成功）。
//
// MAJOR 2 修复：destroy 失败时保留 DB 记录（status=orphaned）+ 保留 key 不吊销，
// 返回错误给调用方。下次 StartAgent 时自动对账清理（见 StartAgent orphaned 分支）。
// destroy 成功后才吊销 key + 清库。
func (s *AgentService) StopAgent(ctx context.Context, userID int64) error {
	existing, err := s.store.GetByUser(ctx, userID)
	if err != nil {
		return err
	}
	if existing == nil {
		return nil // 幂等：无实例 = 成功
	}

	// 1. 销毁容器（manager 侧：rm -f 重试 3 次 + 幂等 + 数据目录保留 7 天）
	destroyErr := s.manager.Destroy(ctx, existing.ContainerName)
	if destroyErr != nil {
		// MAJOR 2: destroy 失败 -> 保留 DB 记录为 orphaned + 保留 key
		// 下次 StartAgent 自动对账（清理旧容器 -> 创建新实例）
		existing.Status = "orphaned"
		_ = s.store.Upsert(ctx, existing)
		return fmt.Errorf("stop agent: destroy failed (record preserved for retry): %w", destroyErr)
	}

	// 2. destroy 成功：吊销用户专属 key
	if err := s.provision.Revoke(ctx, existing.AgentKeyID); err != nil {
		// key 吊销失败不阻塞清库（容器已销毁，key 可后续手动清理）
		// 但记录为 orphaned 以提醒有人工介入
		existing.Status = "orphaned"
		_ = s.store.Upsert(ctx, existing)
		return fmt.Errorf("stop agent: revoke key failed (container destroyed): %w", err)
	}

	// 3. 清库
	if err := s.store.DeleteByUser(ctx, userID); err != nil {
		return fmt.Errorf("stop agent: clear: %w", err)
	}
	return nil
}

// GetAgentStatus 返回用户实例当前状态（无实例 -> not_started）。
func (s *AgentService) GetAgentStatus(ctx context.Context, userID int64) (*AgentState, error) {
	existing, err := s.store.GetByUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return &AgentState{Status: "not_started"}, nil
	}
	return s.stateFromRow(ctx, existing)
}

// stateFromRow 合并数据库记录与 agent-manager 实时状态。
func (s *AgentService) stateFromRow(ctx context.Context, a *Agent) (*AgentState, error) {
	state := &AgentState{
		Status:    a.Status,
		Port:      a.Port,
		AgentURL:  s.agentURL(a.Port),
		CreatedAt: a.CreatedAt.Format(time.RFC3339),
	}

	// orphaned 状态优先返回 DB 记录（manager 可能也不可达）
	if a.Status == "orphaned" {
		return state, nil
	}

	mgr, err := s.manager.Status(ctx, a.ContainerName)
	if err != nil {
		// manager 不可达不算致命：保留库内状态
		return state, nil
	}
	switch mgr.Status {
	case "running":
		state.Status = "running"
		if mgr.Port > 0 {
			state.Port = mgr.Port
			state.AgentURL = s.agentURL(mgr.Port)
		}
	case "stopped", "missing":
		state.Status = "stopped"
		state.Port = 0
		state.AgentURL = ""
	default:
		state.Status = mgr.Status
	}
	return state, nil
}

func (s *AgentService) agentURL(port int) string {
	if port <= 0 {
		return ""
	}
	base := s.cfg.Agent.PublicURLBase
	if base == "" {
		return ""
	}
	return fmt.Sprintf("%s:%d", strings.TrimRight(base, "/"), port)
}
