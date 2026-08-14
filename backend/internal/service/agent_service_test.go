package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

// ──────────────────────────────────────────────────────────────
// AgentService 生命周期单测（mock manager client / provisioner / store）
// 覆盖: start / stop / status / 重复 start 幂等 / 启动失败回滚 key /
//       stop 失败保留 orphaned 记录 / orphaned 记录下次 start 自动对账
// ──────────────────────────────────────────────────────────────

type mockAgentManager struct {
	mu           sync.Mutex
	createCalls  []string
	destroyCalls []string
	statuses     map[string]AgentManagerStatus
	createErr    error
	destroyErr   error // MAJOR 2: 模拟 destroy 失败
}

func newMockAgentManager() *mockAgentManager {
	return &mockAgentManager{statuses: map[string]AgentManagerStatus{}}
}

func (m *mockAgentManager) Create(ctx context.Context, name, apiKey, baseURL, model string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.createErr != nil {
		return 0, m.createErr
	}
	m.createCalls = append(m.createCalls, name)
	port := 18791 + len(m.createCalls)
	m.statuses[name] = AgentManagerStatus{Status: "running", Port: port}
	return port, nil
}

func (m *mockAgentManager) Destroy(ctx context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.destroyCalls = append(m.destroyCalls, name)
	if m.destroyErr != nil {
		return m.destroyErr
	}
	delete(m.statuses, name)
	return nil
}

func (m *mockAgentManager) Status(ctx context.Context, name string) (AgentManagerStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st, ok := m.statuses[name]
	if !ok {
		return AgentManagerStatus{Status: "missing"}, nil
	}
	return st, nil
}

type mockAgentProvisioner struct {
	mu        sync.Mutex
	created   int
	revoked   []int64
	createErr error
}

func (p *mockAgentProvisioner) Create(ctx context.Context, userID int64) (string, int64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.createErr != nil {
		return "", 0, p.createErr
	}
	p.created++
	return "sk-agent-test-" + string(rune('a'+p.created-1)), int64(1000 + p.created), nil
}

func (p *mockAgentProvisioner) Revoke(ctx context.Context, keyID int64) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.revoked = append(p.revoked, keyID)
	return nil
}

type mockAgentStore struct {
	mu   sync.Mutex
	rows map[int64]*Agent
	seq  int64
}

func newMockAgentStore() *mockAgentStore {
	return &mockAgentStore{rows: map[int64]*Agent{}}
}

func (s *mockAgentStore) GetByUser(ctx context.Context, userID int64) (*Agent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a, ok := s.rows[userID]; ok {
		cp := *a
		return &cp, nil
	}
	return nil, nil
}

func (s *mockAgentStore) Upsert(ctx context.Context, a *Agent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	a.ID = s.seq
	a.CreatedAt = time.Now()
	cp := *a
	s.rows[a.UserID] = &cp
	return nil
}

func (s *mockAgentStore) DeleteByUser(ctx context.Context, userID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.rows, userID)
	return nil
}

func newTestAgentService(mgr *mockAgentManager, prov *mockAgentProvisioner, store *mockAgentStore) *AgentService {
	if mgr == nil {
		mgr = newMockAgentManager()
	}
	if prov == nil {
		prov = &mockAgentProvisioner{}
	}
	if store == nil {
		store = newMockAgentStore()
	}
	cfg := &config.Config{}
	cfg.Agent.ManagerURL = "http://127.0.0.1:9180"
	cfg.Agent.ManagerToken = "test-token"
	cfg.Agent.ModelBaseURL = "http://host.docker.internal:18080/v1"
	cfg.Agent.PublicURLBase = "http://192.168.31.90"
	return NewAgentService(store, mgr, prov, cfg)
}

func TestAgentServiceStartLifecycle(t *testing.T) {
	mgr := newMockAgentManager()
	prov := &mockAgentProvisioner{}
	store := newMockAgentStore()
	svc := newTestAgentService(mgr, prov, store)
	ctx := context.Background()

	state, err := svc.StartAgent(ctx, 42)
	if err != nil {
		t.Fatalf("StartAgent: %v", err)
	}
	if state.Status != "running" {
		t.Fatalf("status = %q, want running", state.Status)
	}
	if state.Port != 18792 {
		t.Fatalf("port = %d, want 18792 (first alloc)", state.Port)
	}
	if state.AgentURL != "http://192.168.31.90:18792" {
		t.Fatalf("agent_url = %q", state.AgentURL)
	}

	mgr.mu.Lock()
	created := len(mgr.createCalls)
	mgr.mu.Unlock()
	if created != 1 {
		t.Fatalf("manager create calls = %d, want 1", created)
	}
	if prov.created != 1 {
		t.Fatalf("key created = %d, want 1", prov.created)
	}
	row, _ := store.GetByUser(ctx, 42)
	if row == nil || row.ContainerName != "agent-42" {
		t.Fatalf("store row missing or wrong: %+v", row)
	}
	if row.AgentKey == "" || row.AgentKeyID == 0 {
		t.Fatalf("agent key not persisted: %+v", row)
	}
}

func TestAgentServiceStartIdempotent(t *testing.T) {
	mgr := newMockAgentManager()
	prov := &mockAgentProvisioner{}
	store := newMockAgentStore()
	svc := newTestAgentService(mgr, prov, store)
	ctx := context.Background()

	if _, err := svc.StartAgent(ctx, 7); err != nil {
		t.Fatalf("first start: %v", err)
	}
	second, err := svc.StartAgent(ctx, 7)
	if err != nil {
		t.Fatalf("second start: %v", err)
	}
	if second.Status != "running" {
		t.Fatalf("second start status = %q, want running (existing instance)", second.Status)
	}
	if prov.created != 1 {
		t.Fatalf("key created = %d, want 1 (idempotent, no new key)", prov.created)
	}
	mgr.mu.Lock()
	created := len(mgr.createCalls)
	mgr.mu.Unlock()
	if created != 1 {
		t.Fatalf("manager create calls = %d, want 1 (idempotent)", created)
	}
}

func TestAgentServiceStop(t *testing.T) {
	mgr := newMockAgentManager()
	prov := &mockAgentProvisioner{}
	store := newMockAgentStore()
	svc := newTestAgentService(mgr, prov, store)
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
	destroyed := len(mgr.destroyCalls)
	mgr.mu.Unlock()
	if destroyed != 1 {
		t.Fatalf("manager destroy calls = %d, want 1", destroyed)
	}
	if len(prov.revoked) != 1 || prov.revoked[0] != keyID {
		t.Fatalf("revoked = %v, want [%d]", prov.revoked, keyID)
	}
	row, _ = store.GetByUser(ctx, 9)
	if row != nil {
		t.Fatalf("row still present after stop: %+v", row)
	}

	// 幂等：再次 stop 无实例 -> no-op 不报错
	if err := svc.StopAgent(ctx, 9); err != nil {
		t.Fatalf("second stop: %v", err)
	}
	mgr.mu.Lock()
	destroyed = len(mgr.destroyCalls)
	mgr.mu.Unlock()
	if destroyed != 1 {
		t.Fatalf("manager destroy calls after second stop = %d, want 1 (idempotent)", destroyed)
	}
}

func TestAgentServiceStatus(t *testing.T) {
	svc := newTestAgentService(nil, nil, nil)
	ctx := context.Background()

	// 未启动 -> not_started
	state, err := svc.GetAgentStatus(ctx, 3)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if state.Status != "not_started" {
		t.Fatalf("status = %q, want not_started", state.Status)
	}

	// 启动后 -> running（合并 manager 实时状态）
	if _, err := svc.StartAgent(ctx, 3); err != nil {
		t.Fatalf("start: %v", err)
	}
	state, err = svc.GetAgentStatus(ctx, 3)
	if err != nil {
		t.Fatalf("status after start: %v", err)
	}
	if state.Status != "running" || state.Port != 18792 {
		t.Fatalf("status after start = %+v, want running/18792", state)
	}

	// manager 报告 missing（容器已消失）-> stopped
	mgr := svc.manager.(*mockAgentManager)
	mgr.mu.Lock()
	delete(mgr.statuses, "agent-3")
	mgr.mu.Unlock()
	state, err = svc.GetAgentStatus(ctx, 3)
	if err != nil {
		t.Fatalf("status after container gone: %v", err)
	}
	if state.Status != "stopped" {
		t.Fatalf("status = %q, want stopped when container missing", state.Status)
	}
	if state.Port != 0 || state.AgentURL != "" {
		t.Fatalf("stopped state should clear port/url: %+v", state)
	}
}

func TestAgentServiceStartFailureRevokesKey(t *testing.T) {
	mgr := newMockAgentManager()
	mgr.createErr = errors.New("docker run failed")
	prov := &mockAgentProvisioner{}
	store := newMockAgentStore()
	svc := newTestAgentService(mgr, prov, store)
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

func TestAgentServiceNotConfigured(t *testing.T) {
	svc := NewAgentService(newMockAgentStore(), newMockAgentManager(), &mockAgentProvisioner{}, &config.Config{})
	if _, err := svc.StartAgent(context.Background(), 1); err == nil {
		t.Fatal("StartAgent should fail when agent config missing")
	}
}

// ──────────────────────────────────────────────────────────────
// MAJOR 2: Stop 失败保留 orphaned 记录 + 下次 start 自动对账
// ──────────────────────────────────────────────────────────────

func TestAgentServiceStopFailurePreservesOrphaned(t *testing.T) {
	mgr := newMockAgentManager()
	mgr.destroyErr = errors.New("docker rm failed (manager unreachable)")
	prov := &mockAgentProvisioner{}
	store := newMockAgentStore()
	svc := newTestAgentService(mgr, prov, store)
	ctx := context.Background()

	// 先正常启动
	if _, err := svc.StartAgent(ctx, 11); err != nil {
		t.Fatalf("start: %v", err)
	}
	row, _ := store.GetByUser(ctx, 11)
	if row == nil {
		t.Fatal("row missing after start")
	}
	keyID := row.AgentKeyID

	// stop 失败 -> 应保留 orphaned 记录 + 不吊销 key
	err := svc.StopAgent(ctx, 11)
	if err == nil {
		t.Fatal("StopAgent should fail when manager destroy fails")
	}

	// DB 记录应保留，status = orphaned
	row, _ = store.GetByUser(ctx, 11)
	if row == nil {
		t.Fatal("row should be preserved (orphaned) after stop failure")
	}
	if row.Status != "orphaned" {
		t.Fatalf("status = %q, want orphaned", row.Status)
	}

	// key 不应被吊销（destroy 失败时保留 key）
	if len(prov.revoked) != 0 {
		t.Fatalf("revoked = %v, want 0 (key preserved on destroy failure)", prov.revoked)
	}
	_ = keyID // keyID 保留在 DB 记录中
}

func TestAgentServiceOrphanedReconciledOnNextStart(t *testing.T) {
	mgr := newMockAgentManager()
	prov := &mockAgentProvisioner{}
	store := newMockAgentStore()
	svc := newTestAgentService(mgr, prov, store)
	ctx := context.Background()

	// 模拟 orphaned 记录：手动注入一条
	store.Upsert(ctx, &Agent{
		UserID:        13,
		ContainerName: "agent-13",
		Port:          18800,
		Status:        "orphaned",
		AgentKey:      "sk-old-key",
		AgentKeyID:    999,
	})

	// 清除 destroyErr（下次 destroy 应成功）
	mgr.destroyErr = nil

	// StartAgent 应检测到 orphaned -> 清理旧容器+旧 key -> 创建新实例
	state, err := svc.StartAgent(ctx, 13)
	if err != nil {
		t.Fatalf("start after orphaned: %v", err)
	}
	if state.Status != "running" {
		t.Fatalf("status = %q, want running", state.Status)
	}

	// 旧 key 应被吊销
	foundOld := false
	for _, kid := range prov.revoked {
		if kid == 999 {
			foundOld = true
		}
	}
	if !foundOld {
		t.Fatalf("old orphaned key (id=999) should be revoked, got revoked=%v", prov.revoked)
	}

	// 旧容器应被 destroy（第一次 = 清理 orphaned，第二次不存在=幂等成功）
	mgr.mu.Lock()
	destroyCount := len(mgr.destroyCalls)
	mgr.mu.Unlock()
	if destroyCount < 1 {
		t.Fatalf("expected at least 1 destroy call for orphaned cleanup, got %d", destroyCount)
	}

	// 新 key 应已创建（orphaned 旧 key + 新 key = 2 次创建）
	if prov.created != 1 {
		t.Fatalf("new key created = %d, want 1", prov.created)
	}

	// DB 记录应为新实例（status=running，非 orphaned）
	row, _ := store.GetByUser(ctx, 13)
	if row == nil || row.Status != "running" {
		t.Fatalf("row after reconciliation = %+v, want running", row)
	}
}
