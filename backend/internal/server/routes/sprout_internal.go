package routes

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"

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

// RegisterSproutInternalRoutes registers fork-owned service endpoints.
// Routes remain disabled unless explicitly enabled and protected by a token.
func RegisterSproutInternalRoutes(r *gin.Engine, cfg *config.Config) {
	if cfg == nil || !cfg.Sprout.InternalAPI.Enabled {
		return
	}

	internal := r.Group("/internal/sprout")
	internal.Use(requireSproutInternalAPIToken(cfg.Sprout.InternalAPI.Token))
	internal.GET("/v1/runtime", sproutRuntimeHandler)
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
		},
	})
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
