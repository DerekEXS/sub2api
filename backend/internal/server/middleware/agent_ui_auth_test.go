//go:build unit

package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// newAgentUITestEnv 创建 Agent UI 会话认证中间件测试环境。
// 返回 gin.Engine（/api/v1/agent/ui/launcher-login 挂新中间件，handler 读 subject）
// 与 AuthService（用于生成 Token）。
func newAgentUITestEnv(users map[int64]*service.User) (*gin.Engine, *service.AuthService) {
	gin.SetMode(gin.TestMode)

	cfg := &config.Config{}
	cfg.JWT.Secret = "test-jwt-secret-32bytes-long!!!"
	cfg.JWT.AccessTokenExpireMinutes = 60

	userRepo := &stubJWTUserRepo{users: users}
	authSvc := service.NewAuthService(nil, userRepo, nil, nil, cfg, nil, nil, nil, nil, nil, nil, nil, nil)
	userSvc := service.NewUserService(userRepo, nil, nil, nil)

	r := gin.New()
	ui := r.Group("/api/v1/agent/ui", gin.HandlerFunc(NewAgentUISessionAuth(authSvc, userSvc)))
	{
		ui.GET("/launcher-login", func(c *gin.Context) {
			subject, ok := GetAuthSubjectFromContext(c)
			if !ok {
				c.JSON(http.StatusUnauthorized, gin.H{"code": "NO_SUBJECT"})
				return
			}
			role, _ := GetUserRoleFromContext(c)
			email := c.GetString(ContextKeyAuthEmail)
			c.JSON(http.StatusOK, gin.H{
				"user_id": subject.UserID,
				"role":    role,
				"email":   email,
			})
		})
	}
	return r, authSvc
}

func agentUITestUser() *service.User {
	return &service.User{
		ID:           1,
		Email:        "test@example.com",
		Role:         service.RoleUser,
		Status:       service.StatusActive,
		Concurrency:  5,
		TokenVersion: 1,
	}
}

func TestAgentUIAuth_NoCredentials_401(t *testing.T) {
	router, _ := newAgentUITestEnv(map[int64]*service.User{1: agentUITestUser()})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/agent/ui/launcher-login", nil)
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestAgentUIAuth_Bearer_Valid(t *testing.T) {
	router, authSvc := newAgentUITestEnv(map[int64]*service.User{1: agentUITestUser()})

	token, err := authSvc.GenerateToken(context.Background(), agentUITestUser())
	require.NoError(t, err)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/agent/ui/launcher-login", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	require.Contains(t, w.Body.String(), `"user_id":1`)
}

func TestAgentUIAuth_Bearer_Invalid_401(t *testing.T) {
	router, _ := newAgentUITestEnv(map[int64]*service.User{1: agentUITestUser()})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/agent/ui/launcher-login", nil)
	req.Header.Set("Authorization", "Bearer not-a-real-token")
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestAgentUIAuth_CzToken_MintsCookie_AndReusable(t *testing.T) {
	router, authSvc := newAgentUITestEnv(map[int64]*service.User{1: agentUITestUser()})

	token, err := authSvc.GenerateToken(context.Background(), agentUITestUser())
	require.NoError(t, err)

	// 通道 b：?cz_token= 换 cookie（首个请求即设置 subject，handler 返回 200）
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/agent/ui/launcher-login?cz_token="+token, nil)
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	require.Contains(t, w.Body.String(), `"user_id":1`)

	setCookies := w.Result().Cookies()
	var sid string
	for _, ck := range setCookies {
		if ck.Name == agentUISessionCookieName {
			sid = ck.Value
			require.False(t, ck.HttpOnly == false, "cookie must be HttpOnly")
			require.Equal(t, "/api/v1/agent/ui", ck.Path)
			require.Equal(t, http.SameSiteLaxMode, ck.SameSite)
		}
	}
	require.NotEmpty(t, sid, "expected cz_ui_session cookie")

	// 通道 c：cookie 复用（不再带 cz_token）
	w2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/agent/ui/launcher-login", nil)
	req2.AddCookie(&http.Cookie{Name: agentUISessionCookieName, Value: sid})
	router.ServeHTTP(w2, req2)

	require.Equal(t, http.StatusOK, w2.Code)
	require.Contains(t, w2.Body.String(), `"user_id":1`)
	require.Contains(t, w2.Body.String(), `"email":"test@example.com"`)
}

func TestAgentUIAuth_CzToken_Invalid_401(t *testing.T) {
	router, _ := newAgentUITestEnv(map[int64]*service.User{1: agentUITestUser()})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/agent/ui/launcher-login?cz_token=bad-token", nil)
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestAgentUIAuth_Cookie_Expired_401_AndLazyCleanup(t *testing.T) {
	router, _ := newAgentUITestEnv(map[int64]*service.User{1: agentUITestUser()})

	// 直接注入一条已过期会话
	sid := "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	agentUISessions.Store(sid, agentUISession{userID: 1, exp: time.Now().Add(-time.Hour).Unix()})
	defer agentUISessions.Delete(sid)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/agent/ui/launcher-login", nil)
	req.AddCookie(&http.Cookie{Name: agentUISessionCookieName, Value: sid})
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusUnauthorized, w.Code)
	_, stillExists := agentUISessions.Load(sid)
	require.False(t, stillExists, "expired session should be lazily deleted")
}

func TestAgentUIAuth_Cookie_InactiveUser_401(t *testing.T) {
	banned := agentUITestUser()
	banned.Status = service.StatusDisabled
	router, _ := newAgentUITestEnv(map[int64]*service.User{1: banned})

	sid := "feedbeeffeedbeeffeedbeeffeedbeeffeedbeeffeedbeeffeedbeeffeedbeef"
	agentUISessions.Store(sid, agentUISession{userID: 1, exp: time.Now().Add(time.Hour).Unix()})
	defer agentUISessions.Delete(sid)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/agent/ui/launcher-login", nil)
	req.AddCookie(&http.Cookie{Name: agentUISessionCookieName, Value: sid})
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestAgentUIAuth_Cookie_UnknownSid_401(t *testing.T) {
	router, _ := newAgentUITestEnv(map[int64]*service.User{1: agentUITestUser()})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/agent/ui/launcher-login", nil)
	req.AddCookie(&http.Cookie{Name: agentUISessionCookieName, Value: "no-such-sid"})
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestAgentUIAuth_UserNotFound_401(t *testing.T) {
	router, _ := newAgentUITestEnv(map[int64]*service.User{1: agentUITestUser()})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/agent/ui/launcher-login", nil)
	req.AddCookie(&http.Cookie{Name: agentUISessionCookieName, Value: "ghost"})
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusUnauthorized, w.Code)
}
