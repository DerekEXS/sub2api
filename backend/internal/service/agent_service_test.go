package service

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

// ──────────────────────────────────────────────────────────────
// AgentService v2 生命周期单测（mock manager interface / provisioner / store）
// 覆盖: start 201/202/200 / get 无实例/有实例 / stop 幂等 /
//       启动失败回滚 key / manager 500 行保留 error / archive 流式
// ──────────────────────────────────────────────────────────────

type mockAgentManagerV2 struct {
	mu          sync.Mutex
	createCalls []int64
	lastModels  []string
	deleteCalls []int64
	getCalls    []int64
	states      map[int64]*AgentV2State
	createCode  int
	createErr   error
	deleteErr   error
	getErr      error
	listErr     error
	archiveErr  error
	archiveData string
	config      AgentConfig
	overrides   map[int64]map[string]int
}

func (m *mockAgentManagerV2) GetConfig(ctx context.Context) (*AgentConfig, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.config.RetainHours == 0 {
		return &AgentConfig{RetainHours: 72, HardcapHours: 168, WorkspaceQuotaMB: 250, MemoryMB: 96, IdleTimeoutMinutes: 30}, nil
	}
	cp := m.config
	return &cp, nil
}

func (m *mockAgentManagerV2) UpdateConfig(ctx context.Context, cfg AgentConfig) (*AgentConfig, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cfg.RetainHours > 0 {
		m.config.RetainHours = cfg.RetainHours
	}
	if cfg.HardcapHours > 0 {
		m.config.HardcapHours = cfg.HardcapHours
	}
	if cfg.WorkspaceQuotaMB > 0 {
		m.config.WorkspaceQuotaMB = cfg.WorkspaceQuotaMB
	}
	if cfg.MemoryMB > 0 {
		m.config.MemoryMB = cfg.MemoryMB
	}
	if cfg.IdleTimeoutMinutes > 0 {
		m.config.IdleTimeoutMinutes = cfg.IdleTimeoutMinutes
	}
	cp := m.config
	return &cp, nil
}

func (m *mockAgentManagerV2) globalLocked() *AgentConfig {
	if m.config.RetainHours == 0 {
		return &AgentConfig{RetainHours: 72, HardcapHours: 168, WorkspaceQuotaMB: 250, MemoryMB: 96, IdleTimeoutMinutes: 30}
	}
	cp := m.config
	return &cp
}

func (m *mockAgentManagerV2) GetUserConfig(ctx context.Context, userID int64) (*AgentUserConfigResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	g := m.globalLocked()
	ov := m.overrides[userID]
	eff := *g
	if ov != nil {
		if v, ok := ov["retain_hours"]; ok && v > 0 {
			eff.RetainHours = v
		}
		if v, ok := ov["hardcap_hours"]; ok && v > 0 {
			eff.HardcapHours = v
		}
		if v, ok := ov["workspace_quota_mb"]; ok && v > 0 {
			eff.WorkspaceQuotaMB = v
		}
		if v, ok := ov["memory_mb"]; ok && v > 0 {
			eff.MemoryMB = v
		}
		if v, ok := ov["idle_timeout_minutes"]; ok && v > 0 {
			eff.IdleTimeoutMinutes = v
		}
	}
	return &AgentUserConfigResponse{Global: *g, Overrides: ov, Effective: eff}, nil
}

func (m *mockAgentManagerV2) UpdateUserConfig(ctx context.Context, userID int64, overrides map[string]int) (*AgentUserConfigResponse, error) {
	m.mu.Lock()
	if m.overrides == nil {
		m.overrides = map[int64]map[string]int{}
	}
	ov := m.overrides[userID]
	if ov == nil {
		ov = map[string]int{}
	}
	for k, v := range overrides {
		if v <= 0 {
			delete(ov, k)
		} else {
			ov[k] = v
		}
	}
	m.overrides[userID] = ov
	g := m.globalLocked()
	eff := *g
	for k, v := range ov {
		switch k {
		case "retain_hours":
			eff.RetainHours = v
		case "hardcap_hours":
			eff.HardcapHours = v
		case "workspace_quota_mb":
			eff.WorkspaceQuotaMB = v
		case "memory_mb":
			eff.MemoryMB = v
		case "idle_timeout_minutes":
			eff.IdleTimeoutMinutes = v
		}
	}
	m.mu.Unlock()
	return &AgentUserConfigResponse{Global: *g, Overrides: ov, Effective: eff}, nil
}

func newMockAgentManagerV2() *mockAgentManagerV2 {
	return &mockAgentManagerV2{
		states:      map[int64]*AgentV2State{},
		createCode:  201, // 默认 201 active
		archiveData: "fake-tar-gz",
	}
}

func (m *mockAgentManagerV2) Create(ctx context.Context, userID int64, apiKey string, models []string) (*AgentV2State, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.createCalls = append(m.createCalls, userID)
	m.lastModels = append([]string(nil), models...)
	if m.createErr != nil {
		return nil, 0, m.createErr
	}
	if m.createCode == 202 {
		return &AgentV2State{UserID: userID, Status: "queued", Position: 1}, 202, nil
	}
	st := &AgentV2State{
		UserID:             userID,
		Status:             "active",
		Port:               18801 + len(m.createCalls),
		AccessHost:         "agent-" + itoa(userID) + ".agent.cloudzone-api.cyou",
		AccessPassword:     "pwd-test",
		IdleDeadline:       time.Now().Add(time.Hour).Unix(),
		RetainDeadline:     time.Now().Add(72 * time.Hour).Unix(),  // 保留期（每次启动刷新）
		HardcapDeadline:    time.Now().Add(168 * time.Hour).Unix(), // 硬顶（从首次激活起算）
		IdleTimeoutMinutes: 30,
		DataRetentionHours: 72,
	}
	m.states[userID] = st
	return st, m.createCode, nil
}

func (m *mockAgentManagerV2) Get(ctx context.Context, userID int64) (*AgentV2State, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.getCalls = append(m.getCalls, userID)
	if m.getErr != nil {
		return nil, m.getErr
	}
	st, ok := m.states[userID]
	if !ok {
		return nil, nil // 无实例
	}
	return st, nil
}

func (m *mockAgentManagerV2) Delete(ctx context.Context, userID int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deleteCalls = append(m.deleteCalls, userID)
	if m.deleteErr != nil {
		return m.deleteErr
	}
	delete(m.states, userID)
	return nil
}

func (m *mockAgentManagerV2) List(ctx context.Context) (*AgentListResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.listErr != nil {
		return nil, m.listErr
	}
	agents := make([]AgentV2State, 0, len(m.states))
	for _, st := range m.states {
		agents = append(agents, *st)
	}
	return &AgentListResponse{Agents: agents, Pool: AgentPoolStats{FreeGB: 20, Active: len(agents), Queued: 0, Archived: 3}}, nil
}

func (m *mockAgentManagerV2) Metrics(ctx context.Context) (map[string]any, error) {
	return map[string]any{"cpu_percent": 1.0}, nil
}

func (m *mockAgentManagerV2) Archive(ctx context.Context, userID int64) (io.Reader, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.archiveErr != nil {
		return nil, m.archiveErr
	}
	return strings.NewReader(m.archiveData), nil
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

type mockAgentProvisionerV2 struct {
	mu        sync.Mutex
	created   int
	revoked   []int64
	createErr error
}

func (p *mockAgentProvisionerV2) Create(ctx context.Context, userID int64) (string, int64, []string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.createErr != nil {
		return "", 0, nil, p.createErr
	}
	p.created++
	return "sk-agent-test-v2-" + itoa(int64(p.created)), int64(2000 + p.created), []string{"deepseek-v4-flash"}, nil
}

func (p *mockAgentProvisionerV2) CleanupOrphans(ctx context.Context, userID int64) {}

func (p *mockAgentProvisionerV2) Revoke(ctx context.Context, keyID int64) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.revoked = append(p.revoked, keyID)
	return nil
}

type mockAgentStoreV2 struct {
	mu   sync.Mutex
	rows map[int64]*Agent
	seq  int64
}

func newMockAgentStoreV2() *mockAgentStoreV2 {
	return &mockAgentStoreV2{rows: map[int64]*Agent{}}
}

func (s *mockAgentStoreV2) GetByUser(ctx context.Context, userID int64) (*Agent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a, ok := s.rows[userID]; ok {
		cp := *a
		return &cp, nil
	}
	return nil, nil
}

func (s *mockAgentStoreV2) Upsert(ctx context.Context, a *Agent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	a.ID = s.seq
	a.CreatedAt = time.Now()
	cp := *a
	s.rows[a.UserID] = &cp
	return nil
}

func (s *mockAgentStoreV2) DeleteByUser(ctx context.Context, userID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.rows, userID)
	return nil
}

func newTestAgentServiceV2(mgr *mockAgentManagerV2, prov *mockAgentProvisionerV2, store *mockAgentStoreV2) *AgentService {
	if mgr == nil {
		mgr = newMockAgentManagerV2()
	}
	if prov == nil {
		prov = &mockAgentProvisionerV2{}
	}
	if store == nil {
		store = newMockAgentStoreV2()
	}
	cfg := &config.Config{}
	cfg.Agent.ManagerURL = "http://127.0.0.1:9180"
	cfg.Agent.ManagerToken = "test-token"
	cfg.Agent.ModelBaseURL = "http://host.docker.internal:18080/v1"
	cfg.Agent.PublicURLBase = "http://192.168.31.90"
	return NewAgentService(store, mgr, prov, nil, cfg)
}

func TestAgentServiceV2StartActive(t *testing.T) {
	mgr := newMockAgentManagerV2() // 默认 201
	prov := &mockAgentProvisionerV2{}
	store := newMockAgentStoreV2()
	svc := newTestAgentServiceV2(mgr, prov, store)
	ctx := context.Background()

	state, err := svc.StartAgent(ctx, 42)
	if err != nil {
		t.Fatalf("StartAgent: %v", err)
	}
	if state.Status != "active" {
		t.Fatalf("status = %q, want active (201)", state.Status)
	}
	if state.Port != 18802 {
		t.Fatalf("port = %d, want 18802 (first alloc)", state.Port)
	}
	if state.AccessHost == "" || state.AccessPassword == "" {
		t.Fatalf("access_host/password missing: %+v", state)
	}
	if state.IdleDeadline == 0 || state.RetainDeadline == 0 || state.HardcapDeadline == 0 {
		t.Fatalf("deadlines missing: %+v", state)
	}
	if state.AgentURL != "http://192.168.31.90:18802" {
		t.Fatalf("agent_url = %q", state.AgentURL)
	}
	if prov.created != 1 {
		t.Fatalf("key created = %d, want 1", prov.created)
	}
	row, _ := store.GetByUser(ctx, 42)
	if row == nil || row.Status != "active" {
		t.Fatalf("store row missing or wrong: %+v", row)
	}
	if row.AgentKey == "" || row.AgentKeyID == 0 {
		t.Fatalf("agent key not persisted: %+v", row)
	}
}

func TestAgentServiceV2StartQueued(t *testing.T) {
	mgr := newMockAgentManagerV2()
	mgr.createCode = 202
	prov := &mockAgentProvisionerV2{}
	store := newMockAgentStoreV2()
	svc := newTestAgentServiceV2(mgr, prov, store)
	ctx := context.Background()

	state, err := svc.StartAgent(ctx, 7)
	if err != nil {
		t.Fatalf("StartAgent(202): %v", err)
	}
	if state.Status != "queued" {
		t.Fatalf("status = %q, want queued (202)", state.Status)
	}
	if state.Position != 1 {
		t.Fatalf("position = %d, want 1", state.Position)
	}
	row, _ := store.GetByUser(ctx, 7)
	if row == nil || row.Status != "queued" {
		t.Fatalf("store row wrong: %+v", row)
	}
}

func TestAgentServiceV2StartIdempotent(t *testing.T) {
	mgr := newMockAgentManagerV2()
	prov := &mockAgentProvisionerV2{}
	store := newMockAgentStoreV2()
	svc := newTestAgentServiceV2(mgr, prov, store)
	ctx := context.Background()

	if _, err := svc.StartAgent(ctx, 3); err != nil {
		t.Fatalf("first start: %v", err)
	}
	second, err := svc.StartAgent(ctx, 3)
	if err != nil {
		t.Fatalf("second start: %v", err)
	}
	if second.Status != "active" {
		t.Fatalf("second start status = %q, want active (idempotent existing)", second.Status)
	}
	if prov.created != 1 {
		t.Fatalf("key created = %d, want 1 (idempotent)", prov.created)
	}
	mgr.mu.Lock()
	created := len(mgr.createCalls)
	mgr.mu.Unlock()
	if created != 1 {
		t.Fatalf("manager create calls = %d, want 1 (idempotent)", created)
	}
}

func TestAgentServiceV2Stop(t *testing.T) {
	mgr := newMockAgentManagerV2()
	prov := &mockAgentProvisionerV2{}
	store := newMockAgentStoreV2()
	svc := newTestAgentServiceV2(mgr, prov, store)
	ctx := context.Background()

	if _, err := svc.StartAgent(ctx, 9); err != nil {
		t.Fatalf("start: %v", err)
	}
	row, _ := store.GetByUser(ctx, 9)
	if row == nil {
		t.Fatal("row missing after start")
	}
	keyID := row.AgentKeyID

	if err := svc.StopAgent(ctx, 9); err != nil {
		t.Fatalf("stop: %v", err)
	}
	mgr.mu.Lock()
	deleted := len(mgr.deleteCalls)
	mgr.mu.Unlock()
	if deleted != 1 {
		t.Fatalf("manager delete calls = %d, want 1", deleted)
	}
	if len(prov.revoked) != 1 || prov.revoked[0] != keyID {
		t.Fatalf("revoked = %v, want [%d]", prov.revoked, keyID)
	}
	row, _ = store.GetByUser(ctx, 9)
	if row != nil {
		t.Fatalf("row still present after stop: %+v", row)
	}

	// 幂等：再次 stop 无本地行 -> 仍调 manager.Delete（manager 端幂等；#320 修复：
	// 本地行缺失时也必须到达 manager，否则孤儿实例永远销毁不掉）
	if err := svc.StopAgent(ctx, 9); err != nil {
		t.Fatalf("second stop: %v", err)
	}
	mgr.mu.Lock()
	deleted = len(mgr.deleteCalls)
	mgr.mu.Unlock()
	if deleted != 2 {
		t.Fatalf("manager delete calls after second stop = %d, want 2 (must always reach manager)", deleted)
	}
	if len(prov.revoked) != 1 {
		t.Fatalf("revoked after second stop = %v, want still [%d] (no key to revoke without row)", prov.revoked, keyID)
	}
}

func TestAgentServiceV2Status(t *testing.T) {
	svc := newTestAgentServiceV2(nil, nil, nil)
	ctx := context.Background()

	// 未启动 -> not_started
	state, err := svc.GetAgentStatus(ctx, 3)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if state.Status != "not_started" {
		t.Fatalf("status = %q, want not_started", state.Status)
	}

	// 启动后 -> active（合并 manager 实时状态）
	if _, err := svc.StartAgent(ctx, 3); err != nil {
		t.Fatalf("start: %v", err)
	}
	state, err = svc.GetAgentStatus(ctx, 3)
	if err != nil {
		t.Fatalf("status after start: %v", err)
	}
	if state.Status != "active" || state.Port != 18802 {
		t.Fatalf("status after start = %+v, want active/18802", state)
	}

	// manager 无记录（实例已销毁）-> not_started 或库内状态
	mgr, ok := svc.manager.(*mockAgentManagerV2)
	if !ok {
		t.Fatal("manager is not *mockAgentManagerV2")
	}
	mgr.mu.Lock()
	delete(mgr.states, 3)
	mgr.mu.Unlock()
	state, err = svc.GetAgentStatus(ctx, 3)
	if err != nil {
		t.Fatalf("status after container gone: %v", err)
	}
	// manager 无记录 + 本地有行 -> 回退到库内记录（active），不崩溃
	if state == nil {
		t.Fatal("state nil after container gone")
	}
}

func TestAgentServiceV2StartFailureRevokesKey(t *testing.T) {
	mgr := newMockAgentManagerV2()
	mgr.createErr = errors.New("docker run failed")
	prov := &mockAgentProvisionerV2{}
	store := newMockAgentStoreV2()
	svc := newTestAgentServiceV2(mgr, prov, store)
	ctx := context.Background()

	if _, err := svc.StartAgent(ctx, 5); err == nil {
		t.Fatal("StartAgent should fail when manager create fails")
	}
	if len(prov.revoked) != 1 {
		t.Fatalf("revoked = %v, want 1 (key must be revoked on failure)", prov.revoked)
	}
	row, _ := store.GetByUser(ctx, 5)
	if row != nil {
		t.Fatalf("no row should persist on failure: %+v", row)
	}
}

func TestAgentServiceV2Manager500PreservesErrorRow(t *testing.T) {
	mgr := newMockAgentManagerV2()
	prov := &mockAgentProvisionerV2{}
	store := newMockAgentStoreV2()
	svc := newTestAgentServiceV2(mgr, prov, store)
	ctx := context.Background()

	// 正常启动
	if _, err := svc.StartAgent(ctx, 11); err != nil {
		t.Fatalf("start: %v", err)
	}
	row, _ := store.GetByUser(ctx, 11)
	if row == nil {
		t.Fatal("row missing after start")
	}

	// stop 失败（manager 500）-> 应保留行 status=error + 不吊销 key
	mgr.deleteErr = errors.New("agent-manager delete: http 500: internal")
	err := svc.StopAgent(ctx, 11)
	if err == nil {
		t.Fatal("StopAgent should fail when manager delete fails")
	}
	row, _ = store.GetByUser(ctx, 11)
	if row == nil {
		t.Fatal("row should be preserved (error) after stop failure")
	}
	if row.Status != "error" {
		t.Fatalf("status = %q, want error", row.Status)
	}
	if len(prov.revoked) != 0 {
		t.Fatalf("revoked = %v, want 0 (key preserved on delete failure)", prov.revoked)
	}
}

func TestAgentServiceV2ListAndArchive(t *testing.T) {
	mgr := newMockAgentManagerV2()
	prov := &mockAgentProvisionerV2{}
	store := newMockAgentStoreV2()
	svc := newTestAgentServiceV2(mgr, prov, store)
	ctx := context.Background()

	// List
	lst, err := svc.ListAgents(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if lst == nil {
		t.Fatal("list nil")
	}
	if len(lst.Agents) != 0 {
		t.Fatalf("agents = %d, want 0 (empty)", len(lst.Agents))
	}
	if lst.Pool.FreeGB != 20 || lst.Pool.Archived != 3 {
		t.Fatalf("pool stats wrong: %+v", lst.Pool)
	}

	// 启动一个后 List 有内容
	if _, err := svc.StartAgent(ctx, 21); err != nil {
		t.Fatalf("start: %v", err)
	}
	lst, _ = svc.ListAgents(ctx)
	if len(lst.Agents) != 1 {
		t.Fatalf("agents = %d, want 1", len(lst.Agents))
	}

	// Archive 流式
	r, err := svc.DownloadArchive(ctx, 21)
	if err != nil {
		t.Fatalf("archive: %v", err)
	}
	data, _ := io.ReadAll(r)
	if string(data) != "fake-tar-gz" {
		t.Fatalf("archive data = %q, want fake-tar-gz", string(data))
	}
}

func TestAgentServiceV2NotConfigured(t *testing.T) {
	svc := NewAgentService(newMockAgentStoreV2(), newMockAgentManagerV2(), &mockAgentProvisionerV2{}, nil, &config.Config{})
	if _, err := svc.StartAgent(context.Background(), 1); err == nil {
		t.Fatal("StartAgent should fail when agent config missing")
	}
}

func TestAgentServiceV2Config(t *testing.T) {
	mgr := newMockAgentManagerV2()
	prov := &mockAgentProvisionerV2{}
	store := newMockAgentStoreV2()
	svc := newTestAgentServiceV2(mgr, prov, store)
	ctx := context.Background()

	// 全局配置默认值（双轨：retain 72 / hardcap 168，含 idle_timeout_minutes 默认 30）
	cfg, err := svc.GetAgentConfig(ctx)
	if err != nil {
		t.Fatalf("get config: %v", err)
	}
	if cfg.RetainHours != 72 || cfg.HardcapHours != 168 || cfg.WorkspaceQuotaMB != 250 || cfg.MemoryMB != 96 || cfg.IdleTimeoutMinutes != 30 {
		t.Fatalf("default config = %+v, want 72/168/250/96/30", cfg)
	}

	// 更新全局配置（retain/hardcap 双键）
	upd, err := svc.UpdateAgentConfig(ctx, AgentConfig{RetainHours: 48, HardcapHours: 120, WorkspaceQuotaMB: 512, MemoryMB: 128, IdleTimeoutMinutes: 45})
	if err != nil {
		t.Fatalf("update config: %v", err)
	}
	if upd.RetainHours != 48 || upd.HardcapHours != 120 || upd.MemoryMB != 128 || upd.IdleTimeoutMinutes != 45 {
		t.Fatalf("updated config = %+v", upd)
	}

	// 每用户覆盖
	ucfg, err := svc.UpdateAgentUserConfig(ctx, 42, map[string]int{"memory_mb": 256})
	if err != nil {
		t.Fatalf("update user config: %v", err)
	}
	if ucfg.Effective.MemoryMB != 256 || ucfg.Effective.WorkspaceQuotaMB != 512 {
		t.Fatalf("effective = %+v, want memory 256 (override) + quota 512 (global)", ucfg.Effective)
	}

	// 每用户 idle 覆盖
	ucfg3, err := svc.UpdateAgentUserConfig(ctx, 42, map[string]int{"idle_timeout_minutes": 15})
	if err != nil {
		t.Fatalf("update user idle override: %v", err)
	}
	if ucfg3.Effective.IdleTimeoutMinutes != 15 {
		t.Fatalf("effective idle = %+v, want 15 (override)", ucfg3.Effective)
	}

	// 清除覆盖回退全局
	ucfg2, err := svc.UpdateAgentUserConfig(ctx, 42, map[string]int{"memory_mb": 0, "idle_timeout_minutes": 0})
	if err != nil {
		t.Fatalf("clear override: %v", err)
	}
	if ucfg2.Effective.MemoryMB != 128 || ucfg2.Effective.IdleTimeoutMinutes != 45 {
		t.Fatalf("effective after clear = %+v, want memory 128 + idle 45 (global)", ucfg2.Effective)
	}
}

func TestPickAgentGroup(t *testing.T) {
	mk := func(id int64, platform string, rate float64) Group {
		return Group{ID: id, Platform: platform, RateMultiplier: rate}
	}
	// 空列表 -> (_, false)
	if _, ok := pickAgentGroup(nil); ok {
		t.Fatal("empty list should return ok=false")
	}

	// 只有 openai：选倍率最低
	got, ok := pickAgentGroup([]Group{mk(1, "anthropic", 0.4), mk(2, "openai", 0.1), mk(3, "openai", 0.3)})
	if !ok || got.ID != 2 {
		t.Fatalf("want openai rate-lowest group 2, got %+v ok=%v", got, ok)
	}

	// 无 openai：任选倍率最低
	got, ok = pickAgentGroup([]Group{mk(1, "anthropic", 0.9), mk(2, "grok", 0.2), mk(3, "anthropic", 0.5)})
	if !ok || got.ID != 2 {
		t.Fatalf("want rate-lowest group 2 (no openai), got %+v ok=%v", got, ok)
	}

	// openai 倍率高于 anthropic：仍优先 openai
	got, ok = pickAgentGroup([]Group{mk(1, "anthropic", 0.05), mk(2, "openai", 0.5)})
	if !ok || got.ID != 2 {
		t.Fatalf("want openai group 2 despite higher rate, got %+v ok=%v", got, ok)
	}

	// 同平台同倍率：ID 小者先
	got, ok = pickAgentGroup([]Group{mk(5, "openai", 0.1), mk(3, "openai", 0.1)})
	if !ok || got.ID != 3 {
		t.Fatalf("want ID 3 on tie, got %+v ok=%v", got, ok)
	}
}

func TestAgentServiceV2StartPassesModels(t *testing.T) {
	mgr := newMockAgentManagerV2()
	prov := &mockAgentProvisionerV2{}
	store := newMockAgentStoreV2()
	svc := newTestAgentServiceV2(mgr, prov, store)
	ctx := context.Background()

	if _, err := svc.StartAgent(ctx, 42); err != nil {
		t.Fatalf("StartAgent: %v", err)
	}
	mgr.mu.Lock()
	models := append([]string(nil), mgr.lastModels...)
	mgr.mu.Unlock()
	if len(models) != 1 || models[0] != "deepseek-v4-flash" {
		t.Fatalf("models passed to manager = %v, want [deepseek-v4-flash]", models)
	}
}
