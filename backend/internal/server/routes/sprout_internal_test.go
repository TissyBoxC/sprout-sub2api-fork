package routes

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestSproutInternalRoutesDisabledByDefault(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	RegisterSproutInternalRoutes(router, &config.Config{}, SproutInternalDependencies{})

	response := httptest.NewRecorder()
	router.ServeHTTP(
		response,
		httptest.NewRequest(http.MethodGet, "/internal/sprout/v1/runtime", nil),
	)

	require.Equal(t, http.StatusNotFound, response.Code)
}

func TestSproutInternalRuntimeRequiresServiceToken(t *testing.T) {
	tests := []struct {
		name   string
		header string
	}{
		{name: "missing token"},
		{name: "wrong token", header: "Bearer " + strings.Repeat("y", 32)},
		{name: "wrong scheme", header: "Basic " + strings.Repeat("x", 32)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			router := newSproutInternalTestRouter()
			request := httptest.NewRequest(
				http.MethodGet,
				"/internal/sprout/v1/runtime",
				nil,
			)
			if tt.header != "" {
				request.Header.Set("Authorization", tt.header)
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)

			require.Equal(t, http.StatusUnauthorized, response.Code)
			require.NotContains(t, response.Body.String(), strings.Repeat("x", 32))

			var envelope sproutResponseEnvelope
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &envelope))
			require.Equal(t, sproutResponseSchemaVersion, envelope.SchemaVersion)
			require.NotEmpty(t, envelope.RequestID)
			require.NotNil(t, envelope.Error)
			require.Equal(t, "unauthenticated", envelope.Error.Code)
		})
	}
}

func TestSproutInternalRuntimeReturnsSafeEnvelope(t *testing.T) {
	router := newSproutInternalTestRouter()
	request := httptest.NewRequest(
		http.MethodGet,
		"/internal/sprout/v1/runtime",
		nil,
	)
	request.Header.Set("Authorization", "Bearer "+strings.Repeat("x", 32))
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	require.Equal(t, http.StatusOK, response.Code)

	var envelope struct {
		SchemaVersion string           `json:"schema_version"`
		RequestID     string           `json:"request_id"`
		Data          map[string]any   `json:"data"`
		Error         *sproutErrorBody `json:"error"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &envelope))
	require.Equal(t, sproutResponseSchemaVersion, envelope.SchemaVersion)
	require.NotEmpty(t, envelope.RequestID)
	require.Nil(t, envelope.Error)
	require.Equal(t, "sprout-sub2api", envelope.Data["service"])
	require.Equal(t, "running", envelope.Data["status"])
	require.NotContains(t, response.Body.String(), strings.Repeat("x", 32))
}

func TestSproutCreateAIAccountRejectsShortPassword(t *testing.T) {
	router := newSproutInternalTestRouter()
	request := httptest.NewRequest(
		http.MethodPost,
		"/internal/sprout/v1/ai-accounts",
		strings.NewReader(`{
			"provider_account_id":"parent_abc",
			"provider_account_email":"parent@example.com",
			"password":"short",
			"concurrency_limit":1
		}`),
	)
	request.Header.Set("Authorization", "Bearer "+strings.Repeat("x", 32))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	require.Equal(t, http.StatusUnprocessableEntity, response.Code)
	require.NotContains(t, response.Body.String(), "short")
}

func TestSproutProviderUserStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		input  string
		want   string
		wantOK bool
	}{
		{name: "empty keeps current status", wantOK: true},
		{name: "active passes through", input: "active", want: "active", wantOK: true},
		{name: "suspended maps to disabled", input: "suspended", want: "disabled", wantOK: true},
		{name: "disabled passes through", input: "disabled", want: "disabled", wantOK: true},
		{name: "unknown is rejected", input: "deleted", wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := sproutProviderUserStatus(tt.input)
			require.Equal(t, tt.wantOK, ok)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestSelectSproutRecommendedModelUsesLowestOperationalLatency(t *testing.T) {
	fast := 120
	slow := 340
	models := []sproutModelLatency{
		{Model: "degraded", Status: "degraded", PrimaryLatencyMs: &fast},
		{Model: "slow", Status: "operational", PrimaryLatencyMs: &slow, RecommendedForNewAccounts: true},
		{Model: "fast", Status: "operational", PrimaryLatencyMs: &fast, RecommendedForNewAccounts: true},
	}

	require.Equal(t, "fast", selectSproutRecommendedModel(models))
}

func TestSelectSproutRecommendedModelReturnsEmptyWithoutOperationalLatency(t *testing.T) {
	latency := 80
	models := []sproutModelLatency{
		{Model: "failed", Status: "failed", PrimaryLatencyMs: &latency},
		{Model: "unknown", Status: "operational"},
	}

	require.Empty(t, selectSproutRecommendedModel(models))
}

// sproutAdminServiceStub 只实现 sprout 内部账号生命周期实际用到的 AdminService
// 方法；其余方法通过内嵌接口保持 nil，未预期调用会 panic，避免测试假绿。
type sproutAdminServiceStub struct {
	service.AdminService

	user            service.User
	lastUpdateUser  *service.UpdateUserInput
	updateUserCalls int

	balanceCalls  int
	balanceUserID int64
	balanceAmount float64
	balanceOp     string
	balanceNotes  string
}

func (s *sproutAdminServiceStub) ListUsers(
	_ context.Context,
	_, _ int,
	_ service.UserListFilters,
	_, _ string,
) ([]service.User, int64, error) {
	return []service.User{s.user}, 1, nil
}

func (s *sproutAdminServiceStub) UpdateUser(
	_ context.Context,
	_ int64,
	input *service.UpdateUserInput,
) (*service.User, error) {
	s.updateUserCalls++
	s.lastUpdateUser = input
	return &s.user, nil
}

func (s *sproutAdminServiceStub) UpdateUserBalance(
	_ context.Context,
	userID int64,
	balance float64,
	operation string,
	notes string,
) (*service.User, error) {
	s.balanceCalls++
	s.balanceUserID = userID
	s.balanceAmount = balance
	s.balanceOp = operation
	s.balanceNotes = notes
	s.user.Balance = balance
	return &s.user, nil
}

func newSproutInternalBalanceTestRouter(stub *sproutAdminServiceStub) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	RegisterSproutInternalRoutes(router, sproutInternalTestConfig(), SproutInternalDependencies{
		AdminService: stub,
	})
	return router
}

func sproutInternalAuthorizedRequest(method, target, body string) *http.Request {
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+strings.Repeat("x", 32))
	request.Header.Set("Content-Type", "application/json")
	return request
}

func decodeSproutAccountBalance(t *testing.T, response *httptest.ResponseRecorder) float64 {
	t.Helper()
	require.Equal(t, http.StatusOK, response.Code)

	var envelope struct {
		Data  map[string]any   `json:"data"`
		Error *sproutErrorBody `json:"error"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &envelope))
	require.Nil(t, envelope.Error)
	balance, ok := envelope.Data["balance_usd"].(float64)
	require.True(t, ok, "balance_usd missing from response: %s", response.Body.String())
	return balance
}

// 回归：平台更新 AI 账号时 balance_usd 曾只塞进 UpdateUserInput.Balance，
// 而 UpdateUser 写的是整行快照且 UserUpdateFields 不含余额列，额度被静默丢弃。
// 现在必须走原子的 SetBalance，且返回与再次读取都要是新额度。
func TestSproutUpdateAIAccountPersistsBalanceAtomically(t *testing.T) {
	stub := &sproutAdminServiceStub{user: service.User{ID: 7, Username: "sprout_parent_abc", Balance: 0}}
	router := newSproutInternalBalanceTestRouter(stub)

	updateResponse := httptest.NewRecorder()
	router.ServeHTTP(updateResponse, sproutInternalAuthorizedRequest(
		http.MethodPut,
		"/internal/sprout/v1/ai-accounts/parent_abc",
		`{"balance_usd":10}`,
	))

	require.Equal(t, 10.0, decodeSproutAccountBalance(t, updateResponse))
	require.Equal(t, 1, stub.balanceCalls)
	require.Equal(t, int64(7), stub.balanceUserID)
	require.Equal(t, 10.0, stub.balanceAmount)
	require.Equal(t, "set", stub.balanceOp)
	require.NotEmpty(t, stub.balanceNotes)
	require.NotNil(t, stub.lastUpdateUser)
	require.Nil(t, stub.lastUpdateUser.Balance, "余额不应再经 UpdateUser 快照写入")

	readResponse := httptest.NewRecorder()
	router.ServeHTTP(readResponse, sproutInternalAuthorizedRequest(
		http.MethodGet,
		"/internal/sprout/v1/ai-accounts/parent_abc",
		"",
	))
	require.Equal(t, 10.0, decodeSproutAccountBalance(t, readResponse))
}

// 未携带 balance_usd 时不得改动余额：既不调用 SetBalance，也要保持原值。
func TestSproutUpdateAIAccountWithoutBalancePreservesBalance(t *testing.T) {
	stub := &sproutAdminServiceStub{user: service.User{ID: 7, Username: "sprout_parent_abc", Balance: 3.5}}
	router := newSproutInternalBalanceTestRouter(stub)

	response := httptest.NewRecorder()
	router.ServeHTTP(response, sproutInternalAuthorizedRequest(
		http.MethodPut,
		"/internal/sprout/v1/ai-accounts/parent_abc",
		`{"status":"active"}`,
	))

	require.Equal(t, 3.5, decodeSproutAccountBalance(t, response))
	require.Zero(t, stub.balanceCalls, "未提供额度时不得调用 SetBalance")
	require.Equal(t, 1, stub.updateUserCalls)
	require.Nil(t, stub.lastUpdateUser.Balance)
}

func sproutInternalTestConfig() *config.Config {
	return &config.Config{
		Sprout: config.SproutConfig{
			InternalAPI: config.SproutInternalAPIConfig{
				Enabled: true,
				Token:   strings.Repeat("x", 32),
			},
		},
	}
}

func newSproutInternalTestRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	RegisterSproutInternalRoutes(router, sproutInternalTestConfig(), SproutInternalDependencies{})
	return router
}
