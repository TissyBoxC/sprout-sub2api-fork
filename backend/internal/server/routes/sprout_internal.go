package routes

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const sproutResponseSchemaVersion = "1.0.0"

type sproutResponseEnvelope struct {
	SchemaVersion string           `json:"schema_version"`
	RequestID     string           `json:"request_id"`
	Data          any              `json:"data"`
	Error         *sproutErrorBody `json:"error"`
}

type sproutErrorBody struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

// SproutInternalDependencies contains existing sub2api services and the
// lookup hook required by the platform-to-gateway account lifecycle API.
//
// The route layer never writes sub2api tables. FindUser must resolve only
// accounts created by the platform's controlled "sprout_" username namespace.
type SproutInternalDependencies struct {
	AdminService          service.AdminService
	APIKeyService         *service.APIKeyService
	SettingService        *service.SettingService
	ChannelMonitorService *service.ChannelMonitorService
}

// RegisterSproutInternalRoutes registers fork-owned service endpoints.
// Routes remain disabled unless explicitly enabled and protected by a token.
func RegisterSproutInternalRoutes(
	r *gin.Engine,
	cfg *config.Config,
	dependencies SproutInternalDependencies,
) {
	if cfg == nil || !cfg.Sprout.InternalAPI.Enabled {
		return
	}

	internal := r.Group("/internal/sprout")
	internal.Use(requireSproutInternalAPIToken(cfg.Sprout.InternalAPI.Token))
	internal.GET("/v1/runtime", sproutRuntimeHandler)
	internal.GET("/v1/runtime-config", sproutRuntimeConfigHandler(dependencies))
	internal.POST("/v1/ai-accounts", sproutCreateAIAccountHandler(dependencies))
	internal.GET("/v1/ai-accounts/:provider_account_id", sproutGetAIAccountHandler(dependencies))
	internal.PUT("/v1/ai-accounts/:provider_account_id", sproutUpdateAIAccountHandler(dependencies))
	internal.POST(
		"/v1/ai-accounts/:provider_account_id/api-keys",
		sproutCreateAPIKeyHandler(dependencies),
	)
	internal.POST(
		"/v1/ai-accounts/:provider_account_id/api-keys/:api_key_id/rotate",
		sproutRotateAPIKeyHandler(dependencies),
	)
	internal.DELETE(
		"/v1/ai-accounts/:provider_account_id/api-keys/:api_key_id",
		sproutDeleteAPIKeyHandler(dependencies),
	)
}

func requireSproutInternalAPIToken(expectedToken string) gin.HandlerFunc {
	return func(c *gin.Context) {
		providedToken, ok := sproutBearerToken(c.GetHeader("Authorization"))
		if !ok || expectedToken == "" ||
			subtle.ConstantTimeCompare([]byte(providedToken), []byte(expectedToken)) != 1 {
			writeSproutError(
				c,
				http.StatusUnauthorized,
				"unauthenticated",
				"服务认证失败",
				false,
			)
			c.Abort()
			return
		}
		c.Next()
	}
}

func sproutRuntimeHandler(c *gin.Context) {
	writeSproutSuccess(c, gin.H{
		"service":          "sprout-sub2api",
		"status":           "running",
		"protocol_version": sproutResponseSchemaVersion,
		"capabilities": []string{
			"request_labels",
			"service_token_auth",
			"ai_account_lifecycle",
			"account_model_allowlist",
			"api_key_lifecycle",
		},
	})
}

type sproutModelLatency struct {
	Model                     string `json:"model"`
	Status                    string `json:"status"`
	PrimaryLatencyMs          *int   `json:"primary_latency_ms,omitempty"`
	AverageLatency7DaysMs     *int   `json:"average_latency_7d_ms,omitempty"`
	RecommendedForNewAccounts bool   `json:"recommended_for_new_accounts"`
}

func sproutRuntimeConfigHandler(
	dependencies SproutInternalDependencies,
) gin.HandlerFunc {
	return func(c *gin.Context) {
		if dependencies.SettingService == nil {
			writeSproutError(c, http.StatusServiceUnavailable, "service_unavailable", "配置服务暂不可用", true)
			return
		}

		defaultBalance := dependencies.SettingService.GetDefaultBalance(c.Request.Context())
		defaultConcurrency := dependencies.SettingService.GetDefaultConcurrency(c.Request.Context())
		models := make([]sproutModelLatency, 0)

		if dependencies.ChannelMonitorService != nil {
			views, err := dependencies.ChannelMonitorService.ListUserView(c.Request.Context())
			if err != nil {
				writeSproutServiceError(c, err, "读取模型延迟失败", true)
				return
			}
			for _, view := range views {
				if view == nil || strings.TrimSpace(view.PrimaryModel) == "" {
					continue
				}
				models = append(models, sproutModelLatency{
					Model:            view.PrimaryModel,
					Status:           view.PrimaryStatus,
					PrimaryLatencyMs: view.PrimaryLatencyMs,
					RecommendedForNewAccounts: view.PrimaryStatus == service.MonitorStatusOperational &&
						view.PrimaryLatencyMs != nil,
				})
				for _, extra := range view.ExtraModels {
					if strings.TrimSpace(extra.Model) == "" {
						continue
					}
					models = append(models, sproutModelLatency{
						Model:            extra.Model,
						Status:           extra.Status,
						PrimaryLatencyMs: extra.LatencyMs,
					})
				}
			}
		}

		recommendedModel := selectSproutRecommendedModel(models)
		for index := range models {
			models[index].RecommendedForNewAccounts =
				models[index].Model == recommendedModel && recommendedModel != ""
		}

		writeSproutSuccess(c, gin.H{
			"default_balance_usd": defaultBalance,
			"default_concurrency": defaultConcurrency,
			"models":              models,
			"recommended_model":   recommendedModel,
		})
	}
}

func selectSproutRecommendedModel(models []sproutModelLatency) string {
	recommendedModel := ""
	recommendedLatency := 0
	for index := range models {
		model := models[index]
		if !model.RecommendedForNewAccounts || model.PrimaryLatencyMs == nil {
			continue
		}
		latency := *model.PrimaryLatencyMs
		if recommendedModel == "" || latency < recommendedLatency {
			recommendedModel = model.Model
			recommendedLatency = latency
		}
	}
	return recommendedModel
}

type sproutCreateAIAccountRequest struct {
	ProviderAccountID    string   `json:"provider_account_id"`
	ProviderAccountEmail string   `json:"provider_account_email"`
	Username             string   `json:"username"`
	Password             string   `json:"password"`
	BalanceUSD           *float64 `json:"balance_usd"`
	ConcurrencyLimit     int      `json:"concurrency_limit"`
	AllowedModels        []string `json:"allowed_models"`
}

type sproutUpdateAIAccountRequest struct {
	Status           *string   `json:"status"`
	BalanceUSD       *float64  `json:"balance_usd"`
	ConcurrencyLimit *int      `json:"concurrency_limit"`
	AllowedModels    *[]string `json:"allowed_models"`
	Reason           string    `json:"reason"`
}

type sproutCreateAPIKeyRequest struct {
	Name          string   `json:"name"`
	AllowedModels []string `json:"allowed_models"`
	QuotaUSD      float64  `json:"quota_usd"`
	ExpiresInDays *int     `json:"expires_in_days"`
	RateLimit5h   float64  `json:"rate_limit_5h"`
	RateLimit1d   float64  `json:"rate_limit_1d"`
	RateLimit7d   float64  `json:"rate_limit_7d"`
}

func sproutCreateAIAccountHandler(
	dependencies SproutInternalDependencies,
) gin.HandlerFunc {
	return func(c *gin.Context) {
		var request sproutCreateAIAccountRequest
		if err := c.ShouldBindJSON(&request); err != nil {
			writeSproutError(c, http.StatusBadRequest, "invalid_request", "请求内容不正确", false)
			return
		}
		request.ProviderAccountID = strings.TrimSpace(request.ProviderAccountID)
		request.ProviderAccountEmail = strings.TrimSpace(request.ProviderAccountEmail)
		if request.ProviderAccountID == "" || request.ProviderAccountEmail == "" ||
			len(request.Password) < 16 || request.ConcurrencyLimit <= 0 {
			writeSproutError(c, http.StatusUnprocessableEntity, "validation_failed", "账号参数不完整", false)
			return
		}
		if dependencies.AdminService == nil {
			writeSproutError(c, http.StatusServiceUnavailable, "service_unavailable", "账号服务暂不可用", true)
			return
		}

		user, err := dependencies.AdminService.CreateUser(c.Request.Context(), &service.CreateUserInput{
			Email:         request.ProviderAccountEmail,
			Password:      request.Password,
			Username:      sproutProviderUsername(request.Username, request.ProviderAccountID),
			Notes:         "managed by sprout-device-platform:" + request.ProviderAccountID,
			Balance:       request.BalanceUSD,
			Concurrency:   request.ConcurrencyLimit,
			AllowedModels: request.AllowedModels,
			Role:          service.RoleUser,
		})
		if err != nil {
			writeSproutServiceError(c, err, "创建 AI 账号失败", false)
			return
		}

		writeSproutSuccess(c, gin.H{
			"provider_account_id": request.ProviderAccountID,
			"user_id":             user.ID,
			"status":              user.Status,
			"balance_usd":         user.Balance,
			"concurrency_limit":   user.Concurrency,
			"allowed_models":      request.AllowedModels,
		})
	}
}

func sproutGetAIAccountHandler(
	dependencies SproutInternalDependencies,
) gin.HandlerFunc {
	return func(c *gin.Context) {
		user, err := sproutFindAIAccount(c, dependencies)
		if err != nil {
			writeSproutServiceError(c, err, "读取 AI 账号失败", true)
			return
		}
		writeSproutSuccess(c, sproutAIAccountResponse(user))
	}
}

func sproutUpdateAIAccountHandler(
	dependencies SproutInternalDependencies,
) gin.HandlerFunc {
	return func(c *gin.Context) {
		user, err := sproutFindAIAccount(c, dependencies)
		if err != nil {
			writeSproutServiceError(c, err, "读取 AI 账号失败", true)
			return
		}
		var request sproutUpdateAIAccountRequest
		if err := c.ShouldBindJSON(&request); err != nil {
			writeSproutError(c, http.StatusBadRequest, "invalid_request", "请求内容不正确", false)
			return
		}
		status := ""
		if request.Status != nil {
			var valid bool
			status, valid = sproutProviderUserStatus(*request.Status)
			if !valid {
				writeSproutError(c, http.StatusUnprocessableEntity, "validation_failed", "账号状态不正确", false)
				return
			}
		}
		updated, err := dependencies.AdminService.UpdateUser(
			c.Request.Context(),
			user.ID,
			&service.UpdateUserInput{
				Status:        status,
				Concurrency:   request.ConcurrencyLimit,
				AllowedModels: request.AllowedModels,
			},
		)
		if err != nil {
			writeSproutServiceError(c, err, "更新 AI 账号失败", false)
			return
		}
		// 余额必须走原子的 SetBalance：UpdateUser 写的是整行快照，用它顺带改余额会
		// 覆盖并发的计费扣款。先落状态/并发/模型等非余额字段，再原子设余额。
		if request.BalanceUSD != nil {
			updated, err = dependencies.AdminService.UpdateUserBalance(
				c.Request.Context(),
				user.ID,
				*request.BalanceUSD,
				"set",
				"sprout platform admin update",
			)
			if err != nil {
				writeSproutServiceError(c, err, "更新 AI 账号失败", false)
				return
			}
		}
		writeSproutSuccess(c, sproutAIAccountResponse(updated))
	}
}

func sproutCreateAPIKeyHandler(
	dependencies SproutInternalDependencies,
) gin.HandlerFunc {
	return func(c *gin.Context) {
		user, err := sproutFindAIAccount(c, dependencies)
		if err != nil {
			writeSproutServiceError(c, err, "读取 AI 账号失败", true)
			return
		}
		if dependencies.APIKeyService == nil {
			writeSproutError(c, http.StatusServiceUnavailable, "service_unavailable", "密钥服务暂不可用", true)
			return
		}
		var request sproutCreateAPIKeyRequest
		if err := c.ShouldBindJSON(&request); err != nil {
			writeSproutError(c, http.StatusBadRequest, "invalid_request", "请求内容不正确", false)
			return
		}
		key, err := dependencies.APIKeyService.Create(
			c.Request.Context(),
			user.ID,
			service.CreateAPIKeyRequest{
				Name:          strings.TrimSpace(request.Name),
				Quota:         request.QuotaUSD,
				ExpiresInDays: request.ExpiresInDays,
				RateLimit5h:   request.RateLimit5h,
				RateLimit1d:   request.RateLimit1d,
				RateLimit7d:   request.RateLimit7d,
			},
		)
		if err != nil {
			writeSproutServiceError(c, err, "创建 AI 密钥失败", false)
			return
		}
		writeSproutSuccess(c, sproutAPIKeyResponse(user.ID, key))
	}
}

func sproutRotateAPIKeyHandler(
	dependencies SproutInternalDependencies,
) gin.HandlerFunc {
	return func(c *gin.Context) {
		user, err := sproutFindAIAccount(c, dependencies)
		if err != nil {
			writeSproutServiceError(c, err, "读取 AI 账号失败", true)
			return
		}
		apiKeyID, err := parseSproutPositiveID(c.Param("api_key_id"))
		if err != nil {
			writeSproutError(c, http.StatusBadRequest, "invalid_request", "密钥编号不正确", false)
			return
		}
		if dependencies.APIKeyService == nil {
			writeSproutError(c, http.StatusServiceUnavailable, "service_unavailable", "密钥服务暂不可用", true)
			return
		}
		existing, err := dependencies.APIKeyService.GetByID(c.Request.Context(), apiKeyID)
		if err != nil || existing.UserID != user.ID {
			writeSproutError(c, http.StatusNotFound, "not_found", "没有找到这条内容", false)
			return
		}
		replacement, err := dependencies.APIKeyService.Create(
			c.Request.Context(),
			user.ID,
			service.CreateAPIKeyRequest{
				Name:          existing.Name,
				GroupID:       existing.GroupID,
				Quota:         existing.Quota,
				ExpiresInDays: sproutExpiresInDays(existing.ExpiresAt),
				RateLimit5h:   existing.RateLimit5h,
				RateLimit1d:   existing.RateLimit1d,
				RateLimit7d:   existing.RateLimit7d,
			},
		)
		if err != nil {
			writeSproutServiceError(c, err, "轮换 AI 密钥失败", true)
			return
		}
		// Keep the old credential valid until the replacement exists so a
		// transient create failure cannot leave the platform without a key.
		if err := dependencies.APIKeyService.Delete(c.Request.Context(), existing.ID, user.ID); err != nil {
			if rollbackErr := dependencies.APIKeyService.Delete(
				c.Request.Context(),
				replacement.ID,
				user.ID,
			); rollbackErr != nil {
				writeSproutError(
					c,
					http.StatusServiceUnavailable,
					"key_rotation_incomplete",
					"密钥轮换未完成，请稍后重试",
					true,
				)
				return
			}
			writeSproutServiceError(c, err, "轮换 AI 密钥失败", true)
			return
		}
		writeSproutSuccess(c, sproutAPIKeyResponse(user.ID, replacement))
	}
}

func sproutDeleteAPIKeyHandler(
	dependencies SproutInternalDependencies,
) gin.HandlerFunc {
	return func(c *gin.Context) {
		user, err := sproutFindAIAccount(c, dependencies)
		if err != nil {
			writeSproutServiceError(c, err, "读取 AI 账号失败", true)
			return
		}
		apiKeyID, err := parseSproutPositiveID(c.Param("api_key_id"))
		if err != nil {
			writeSproutError(c, http.StatusBadRequest, "invalid_request", "密钥编号不正确", false)
			return
		}
		if dependencies.APIKeyService == nil {
			writeSproutError(c, http.StatusServiceUnavailable, "service_unavailable", "密钥服务暂不可用", true)
			return
		}
		if err := dependencies.APIKeyService.Delete(c.Request.Context(), apiKeyID, user.ID); err != nil {
			writeSproutServiceError(c, err, "删除 AI 密钥失败", true)
			return
		}
		writeSproutSuccess(c, gin.H{"deleted": true})
	}
}

func sproutFindAIAccount(
	c *gin.Context,
	dependencies SproutInternalDependencies,
) (*service.User, error) {
	providerAccountID := strings.TrimSpace(c.Param("provider_account_id"))
	if providerAccountID == "" {
		return nil, errors.New("provider account id is required")
	}
	if dependencies.AdminService == nil {
		return nil, errors.New("admin service is not configured")
	}
	users, _, err := dependencies.AdminService.ListUsers(
		c.Request.Context(),
		1,
		20,
		service.UserListFilters{Search: "sprout_" + providerAccountID},
		"",
		"",
	)
	if err != nil {
		return nil, err
	}
	for index := range users {
		if users[index].Username == "sprout_"+providerAccountID {
			return &users[index], nil
		}
	}
	return nil, service.ErrUserNotFound
}

func sproutAIAccountResponse(user *service.User) gin.H {
	return gin.H{
		"provider_account_id": sproutProviderAccountID(user),
		"user_id":             user.ID,
		"status":              user.Status,
		"balance_usd":         user.Balance,
		"concurrency_limit":   user.Concurrency,
		"allowed_models":      user.AllowedModels,
		"allowed_groups":      user.AllowedGroups,
	}
}

func sproutAPIKeyResponse(userID int64, key *service.APIKey) gin.H {
	return gin.H{
		"user_id":    userID,
		"api_key_id": key.ID,
		"name":       key.Name,
		"api_key":    key.Key,
		"status":     key.Status,
		"quota_usd":  key.Quota,
		"expires_at": key.ExpiresAt,
	}
}

func sproutProviderUsername(username string, providerAccountID string) string {
	trimmed := strings.TrimSpace(username)
	if trimmed != "" {
		return trimmed
	}
	return "sprout_" + providerAccountID
}

// sproutProviderUserStatus maps the platform lifecycle vocabulary to the
// upstream sub2api user status. Empty input means "leave unchanged".
func sproutProviderUserStatus(status string) (string, bool) {
	switch strings.TrimSpace(status) {
	case "":
		return "", true
	case service.StatusActive:
		return service.StatusActive, true
	case "suspended", service.StatusDisabled:
		return service.StatusDisabled, true
	default:
		return "", false
	}
}

func sproutProviderAccountID(user *service.User) string {
	if user == nil {
		return ""
	}
	const prefix = "sprout_"
	if strings.HasPrefix(user.Username, prefix) {
		return strings.TrimPrefix(user.Username, prefix)
	}
	return strings.TrimSpace(user.Notes)
}

func parseSproutPositiveID(value string) (int64, error) {
	parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil || parsed <= 0 {
		return 0, errors.New("invalid identifier")
	}
	return parsed, nil
}

func sproutExpiresInDays(expiresAt *time.Time) *int {
	if expiresAt == nil {
		return nil
	}
	days := int(time.Until(*expiresAt).Hours() / 24)
	if days < 1 {
		days = 1
	}
	return &days
}

func writeSproutServiceError(
	c *gin.Context,
	err error,
	message string,
	retryable bool,
) {
	status := http.StatusBadRequest
	if errors.Is(err, service.ErrUserNotFound) {
		status = http.StatusNotFound
	}
	writeSproutError(c, status, "operation_failed", message, retryable)
}

func writeSproutSuccess(c *gin.Context, data any) {
	c.JSON(http.StatusOK, sproutResponseEnvelope{
		SchemaVersion: sproutResponseSchemaVersion,
		RequestID:     sproutRequestID(c),
		Data:          data,
		Error:         nil,
	})
}

func writeSproutError(
	c *gin.Context,
	status int,
	code string,
	message string,
	retryable bool,
) {
	c.JSON(status, sproutResponseEnvelope{
		SchemaVersion: sproutResponseSchemaVersion,
		RequestID:     sproutRequestID(c),
		Data:          nil,
		Error: &sproutErrorBody{
			Code:      code,
			Message:   message,
			Retryable: retryable,
		},
	})
}

func sproutRequestID(c *gin.Context) string {
	if c != nil && c.Request != nil {
		if requestID, ok := c.Request.Context().Value(ctxkey.RequestID).(string); ok {
			requestID = strings.TrimSpace(requestID)
			if requestID != "" {
				return requestID
			}
		}
		return uuid.NewString()
	}
	return uuid.NewString()
}

func sproutBearerToken(header string) (string, bool) {
	parts := strings.SplitN(strings.TrimSpace(header), " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return "", false
	}
	token := strings.TrimSpace(parts[1])
	return token, token != ""
}
