package routes

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	servermiddleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func newSproutChildPolicyGatewayRouter(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{
		Gateway: config.GatewayConfig{
			MaxBodySize:     1024 * 1024,
			TextMaxBodySize: 1024 * 1024,
		},
	}
	cfg.Sprout.ChildPolicy = config.SproutChildPolicyConfig{
		Enabled:        true,
		DefaultAgeBand: "",
		PolicyVersions: []config.SproutPolicyVersionBinding{
			{Version: "2026.10.1", AgeBand: "6-8"},
		},
		AgeBands: []config.SproutAgeBandPolicy{
			{
				Name:            "6-8",
				AllowedModels:   []string{"gpt-5-mini"},
				MaxOutputTokens: 512,
				ContentRating:   "G",
			},
		},
		InjectSafetyBoundary: true,
		SafetyBoundary:       "CHILD SAFETY BOUNDARY",
	}

	router := gin.New()
	router.Use(servermiddleware.SproutRequestLabelsMiddleware())
	RegisterGatewayRoutes(
		router,
		&handler.Handlers{
			Gateway:       &handler.GatewayHandler{},
			OpenAIGateway: &handler.OpenAIGatewayHandler{},
			AsyncImage:    handler.NewAsyncImageHandler(nil, nil),
		},
		servermiddleware.APIKeyAuthMiddleware(func(c *gin.Context) {
			groupID := int64(1)
			c.Set(string(servermiddleware.ContextKeyAPIKey), &service.APIKey{
				GroupID: &groupID,
				Group:   &service.Group{Platform: service.PlatformOpenAI},
			})
			c.Next()
		}),
		nil,
		nil,
		nil,
		nil,
		nil,
		cfg,
	)
	return router
}

func doSproutPolicyGatewayRequest(router *gin.Engine, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Sprout-Tenant-ID", "family_ab12cd34ef56")
	request.Header.Set("X-Sprout-Device-ID", "device_001")
	request.Header.Set("X-Sprout-Policy-Version", "2026.10.1")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

func TestGatewayChildPolicyRejectsModelOutsideAgeBand(t *testing.T) {
	router := newSproutChildPolicyGatewayRouter(t)

	response := doSproutPolicyGatewayRequest(router, `{"model":"gpt-5.4","messages":[{"role":"user","content":"hi"}]}`)

	require.Equal(t, http.StatusNotFound, response.Code)
	require.Contains(t, response.Body.String(), "not available for this age")
}

func TestGatewayChildPolicyAllowsAgeAppropriateModel(t *testing.T) {
	router := newSproutChildPolicyGatewayRouter(t)

	response := doSproutPolicyGatewayRequest(router, `{"model":"gpt-5-mini","messages":[{"role":"user","content":"hi"}]}`)

	// The stub handler has no upstream configured, so it may fail downstream;
	// the assertion is only that child-policy admission did not reject it.
	require.NotContains(t, response.Body.String(), "not available for this age")
}
