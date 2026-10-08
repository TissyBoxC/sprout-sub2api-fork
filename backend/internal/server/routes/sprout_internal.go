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
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/util/logredact"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const sproutResponseSchemaVersion = "1.0.0"

// sproutInternalIdentityHeaders are end-user identity headers. The internal API
// is service-to-service only; accepting any of these would let a parent or
// child credential masquerade as a trusted platform service.
var sproutInternalIdentityHeaders = []string{
	"X-Sprout-Parent-ID",
	"X-Sprout-Child-ID",
	"X-Sprout-Device-ID",
	"X-Sprout-Account-ID",
	"X-Parent-ID",
	"X-Child-ID",
}

const sproutInternalAuditActor = "sprout-device-platform"

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
	AuditLogService       *service.AuditLogService
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
	internal.Use(rejectSproutEndUserIdentity())
	internal.Use(auditSproutInternalMutation(dependencies.AuditLogService))
	internal.GET("/v1/runtime", sproutRuntimeHandler)
	internal.GET("/v1/runtime-config", sproutRuntimeConfigHandler(dependencies))
	internal.POST("/v1/ai-accounts", sproutCreateAIAccountHandler(dependencies))
	internal.GET("/v1/ai-accounts/:provider_account_id", sproutGetAIAccountHandler(dependencies))
	internal.PUT("/v1/ai-accounts/:provider_account_id", sproutUpdateAIAccountHandler(dependencies))
	internal.DELETE(
		"/v1/ai-accounts/:provider_account_id",
		sproutDeleteAIAccountHandler(dependencies),
	)
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
		if err := config.ValidateSproutInternalAPIToken(expectedToken); err != nil {
			writeSproutError(
				c,
				http.StatusServiceUnavailable,
				"service_auth_not_configured",
				"服务认证暂不可用",
				false,
			)
			c.Abort()
			return
		}
		providedToken, ok := sproutBearerToken(c.GetHeader("Authorization"))
		if !ok || subtle.ConstantTimeCompare([]byte(providedToken), []byte(expectedToken)) != 1 {
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

func rejectSproutEndUserIdentity() gin.HandlerFunc {
	return func(c *gin.Context) {
		for _, header := range sproutInternalIdentityHeaders {
			if strings.TrimSpace(c.GetHeader(header)) != "" {
				writeSproutError(
					c,
					http.StatusForbidden,
					"end_user_identity_forbidden",
					"此服务不允许家长或儿童身份访问",
					false,
				)
				c.Abort()
				return
			}
		}
		c.Next()
	}
}

// auditSproutInternalMutation records sensitive service-to-service writes with
// a stable service actor and request correlation. Account lifecycle payloads
// contain passwords and one-time API keys, so body content is never stored.
func auditSproutInternalMutation(auditService *service.AuditLogService) gin.HandlerFunc {
	return func(c *gin.Context) {
		if auditService == nil || c.Request == nil || c.Request.Method == http.MethodGet {
			c.Next()
			return
		}
		start := time.Now()
		c.Next()

		extra := map[string]any{
			"result": sproutInternalAuditResult(c.Writer.Status()),
		}
		if providerAccountID := strings.TrimSpace(c.Param("provider_account_id")); providerAccountID != "" {
			extra["provider_account_id"] = providerAccountID
		}
		if apiKeyID := strings.TrimSpace(c.Param("api_key_id")); apiKeyID != "" {
			extra["api_key_id"] = apiKeyID
		}
		entry := &service.AuditLog{
			CreatedAt:        time.Now().UTC(),
			ActorEmail:       sproutInternalAuditActor,
			AuthMethod:       "service_token",
			CredentialMasked: middleware2.MaskedRequestCredential(c),
			Action:           sproutInternalAuditAction(c),
			Method:           c.Request.Method,
			Path:             c.FullPath(),
			StatusCode:       c.Writer.Status(),
			LatencyMs:        time.Since(start).Milliseconds(),
			Extra:            extra,
		}
		if c.Request.URL != nil {
			entry.RequestID = sproutRequestID(c)
			entry.ClientIP = c.ClientIP()
			entry.UserAgent = c.Request.UserAgent()
		}
		auditService.Record(entry)
	}
}

func sproutInternalAuditAction(c *gin.Context) string {
	path := c.FullPath()
	switch {
	case strings.HasSuffix(path, "/api-keys") && c.Request.Method == http.MethodPost:
		return "sprout.ai_account.api_key.create"
	case strings.HasSuffix(path, "/rotate"):
		return "sprout.ai_account.api_key.rotate"
	case strings.Contains(path, "/api-keys/") && c.Request.Method == http.MethodDelete:
		return "sprout.ai_account.api_key.delete"
	case c.Request.Method == http.MethodPost && strings.HasSuffix(path, "/ai-accounts"):
		return "sprout.ai_account.create"
	case c.Request.Method == http.MethodPut:
		return "sprout.ai_account.update"
	case c.Request.Method == http.MethodDelete:
		return "sprout.ai_account.delete"
	default:
		return "sprout.internal." + strings.ToLower(c.Request.Method)
	}
}

func sproutInternalAuditResult(status int) string {
	if status >= 200 && status < 400 {
		return "success"
	}
	return "failure"
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
		if !sproutValidProviderAccountID(request.ProviderAccountID) ||
			!sproutControlledProviderEmail(request.ProviderAccountEmail) ||
			len(request.Password) < 16 || request.ConcurrencyLimit <= 0 {
			writeSproutError(c, http.StatusUnprocessableEntity, "validation_failed", "账号参数不完整", false)
			return
		}
		controlledUsername := sproutControlledUsername(request.ProviderAccountID)
		if username := strings.TrimSpace(request.Username); username != "" &&
			username != controlledUsername {
			writeSproutError(c, http.StatusUnprocessableEntity, "validation_failed", "账号用户名不正确", false)
			return
		}
		if dependencies.AdminService == nil {
			writeSproutError(c, http.StatusServiceUnavailable, "service_unavailable", "账号服务暂不可用", true)
			return
		}

		user, err := dependencies.AdminService.CreateUser(c.Request.Context(), &service.CreateUserInput{
			Email:         request.ProviderAccountEmail,
			Password:      request.Password,
			Username:      controlledUsername,
			Notes:         sproutControlledNotes(request.ProviderAccountID),
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

func sproutDeleteAIAccountHandler(
	dependencies SproutInternalDependencies,
) gin.HandlerFunc {
	return func(c *gin.Context) {
		if dependencies.AdminService == nil {
			writeSproutError(c, http.StatusServiceUnavailable, "service_unavailable", "账号服务暂不可用", true)
			return
		}

		providerAccountID := strings.TrimSpace(c.Param("provider_account_id"))
		if !sproutValidProviderAccountID(providerAccountID) {
			writeSproutError(c, http.StatusBadRequest, "invalid_request", "账号参数不正确", false)
			return
		}

		// IncludeDeleted is required for both the lookup and the idempotent
		// retry path: after the first delete the row is soft-deleted and a
		// normal lookup intentionally cannot see it.
		controlledUsername := sproutControlledUsername(providerAccountID)
		controlledNotes := sproutControlledNotes(providerAccountID)
		user, err := sproutFindControlledAIAccount(
			c,
			dependencies,
			controlledUsername,
			controlledNotes,
			true,
		)
		if err != nil {
			if errors.Is(err, service.ErrUserNotFound) {
				writeSproutError(c, http.StatusNotFound, "not_found", "没有找到这条内容", false)
				return
			}
			writeSproutServiceError(c, err, "删除 AI 账号失败", true)
			return
		}
		if user.DeletedAt != nil {
			writeSproutSuccess(c, gin.H{
				"provider_account_id": providerAccountID,
				"deleted":             true,
			})
			return
		}

		if err := dependencies.AdminService.DeleteUser(c.Request.Context(), user.ID); err != nil {
			if errors.Is(err, service.ErrUserNotFound) {
				// Another request may have deleted the row between lookup and
				// delete. Re-resolve with soft-deleted rows included before
				// deciding the retry is safe.
				if _, lookupErr := sproutFindControlledAIAccount(
					c,
					dependencies,
					controlledUsername,
					controlledNotes,
					true,
				); lookupErr == nil {
					writeSproutSuccess(c, gin.H{
						"provider_account_id": providerAccountID,
						"deleted":             true,
					})
					return
				}
			}
			writeSproutServiceError(c, err, "删除 AI 账号失败", true)
			return
		}
		writeSproutSuccess(c, gin.H{
			"provider_account_id": providerAccountID,
			"deleted":             true,
		})
	}
}

// sproutFindControlledAIAccount resolves one exact account created by the
// platform. The username alone is not sufficient: OAuth registration can
// supply arbitrary usernames, so both the exact username and the immutable
// platform ownership note must match. Repository search is intentionally fuzzy
// and is never trusted for authorization decisions.
func sproutFindControlledAIAccount(
	c *gin.Context,
	dependencies SproutInternalDependencies,
	controlledUsername string,
	controlledNotes string,
	includeDeleted bool,
) (*service.User, error) {
	const pageSize = 100
	for page := 1; ; page++ {
		users, total, err := dependencies.AdminService.ListUsers(
			c.Request.Context(),
			page,
			pageSize,
			service.UserListFilters{
				Search:         controlledUsername,
				IncludeDeleted: includeDeleted,
			},
			"",
			"",
		)
		if err != nil {
			return nil, err
		}
		for index := range users {
			if sproutIsControlledAIAccount(&users[index], controlledUsername, controlledNotes) {
				return &users[index], nil
			}
		}
		if len(users) == 0 || int64(page*pageSize) >= total {
			break
		}
	}
	return nil, service.ErrUserNotFound
}

func sproutIsControlledAIAccount(
	user *service.User,
	controlledUsername string,
	controlledNotes string,
) bool {
	if user == nil ||
		user.Username != controlledUsername ||
		user.Notes != controlledNotes {
		return false
	}
	return sproutControlledProviderEmail(user.Email)
}

func sproutControlledProviderEmail(email string) bool {
	return strings.HasSuffix(strings.ToLower(strings.TrimSpace(email)), "@ai.sprout.local")
}

func sproutValidProviderAccountID(providerAccountID string) bool {
	providerAccountID = strings.TrimSpace(providerAccountID)
	if !strings.HasPrefix(providerAccountID, "parent_") {
		return false
	}
	suffix := strings.TrimPrefix(providerAccountID, "parent_")
	if len(suffix) != 32 {
		return false
	}
	for _, r := range suffix {
		if (r >= 'a' && r <= 'f') || (r >= '0' && r <= '9') {
			continue
		}
		return false
	}
	return true
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
	return sproutFindControlledAIAccount(
		c,
		dependencies,
		sproutControlledUsername(providerAccountID),
		sproutControlledNotes(providerAccountID),
		false,
	)
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

func sproutControlledUsername(providerAccountID string) string {
	return "sprout_" + strings.TrimSpace(providerAccountID)
}

func sproutControlledNotes(providerAccountID string) string {
	return "managed by sprout-device-platform:" + strings.TrimSpace(providerAccountID)
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
	} else if errors.Is(err, service.ErrInsufficientPerms) {
		status = http.StatusForbidden
	} else {
		redacted := ""
		if err != nil {
			redacted = strings.ToLower(logredact.RedactText(err.Error()))
		}
		for _, hint := range []string{"connection", "database", "sql", "timeout", "upstream"} {
			if strings.Contains(redacted, hint) {
				status = http.StatusServiceUnavailable
				retryable = true
				break
			}
		}
	}
	writeSproutError(c, status, "operation_failed", message, retryable)
}

func writeSproutSuccess(c *gin.Context, data any) {
	c.Header("Cache-Control", "no-store")
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
	c.Header("Cache-Control", "no-store")
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
