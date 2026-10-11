package middleware

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func sproutChildPolicyTestConfig() *config.Config {
	cfg := &config.Config{}
	cfg.Sprout.ChildPolicy = config.SproutChildPolicyConfig{
		Enabled:              true,
		DefaultAgeBand:       "",
		RequirePolicyVersion: false,
		PolicyVersions: []config.SproutPolicyVersionBinding{
			{Version: "2026.10.1", AgeBand: "6-8"},
			{Version: "2026.10.2", AgeBand: "3-5"},
		},
		AgeBands: []config.SproutAgeBandPolicy{
			{
				Name:            "3-5",
				AllowedModels:   []string{"gpt-5-mini"},
				MaxOutputTokens: 256,
				ContentRating:   "G",
			},
			{
				Name:            "6-8",
				AllowedModels:   []string{"gpt-5-mini", "claude-haiku-4-5"},
				MaxOutputTokens: 512,
				ContentRating:   "G",
			},
		},
		InjectSafetyBoundary: true,
		SafetyBoundary:       "CHILD SAFETY BOUNDARY",
	}
	return cfg
}

// newSproutChildPolicyRouter mounts the labels middleware (so provenance is
// attached) followed by the child-policy middleware, and echoes the rewritten
// body and the resolved decision for assertions.
func newSproutChildPolicyRouter(t *testing.T, cfg *config.Config) (*gin.Engine, *string, *SproutPolicyDecision) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(SproutRequestLabelsMiddleware())
	var capturedBody string
	var capturedDecision SproutPolicyDecision
	// Mount an outer capture middleware so a policy rejection (which aborts the
	// chain) still yields the resolved decision and rewritten body for assertion.
	capture := func(c *gin.Context) {
		c.Next()
		capturedDecision = GetSproutPolicyDecision(c)
		if body, ok := readSproutPolicyBody(c); ok {
			capturedBody = string(body)
		}
	}
	router.Use(capture, SproutChildPolicy(cfg))
	echo := func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	}
	router.POST("/v1/chat/completions", echo)
	router.POST("/v1/messages", echo)
	router.POST("/v1beta/models/:modelAction", echo)
	router.POST("/v1/responses", echo)
	return router, &capturedBody, &capturedDecision
}

func doSproutChildPolicyRequest(router *gin.Engine, path string, headers map[string]string, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

func sproutTaggedHeaders(policyVersion string) map[string]string {
	headers := map[string]string{
		sproutTenantIDHeader: "family_ab12cd34ef56",
		sproutDeviceIDHeader: "device_001",
		sproutProjectHeader:  "voice",
	}
	if policyVersion != "" {
		headers[sproutPolicyVersionHeader] = policyVersion
	}
	return headers
}

func TestSproutChildPolicyDisabledLeavesRequestsUntouched(t *testing.T) {
	cfg := &config.Config{}
	router, body, _ := newSproutChildPolicyRouter(t, cfg)

	response := doSproutChildPolicyRequest(router, "/v1/chat/completions", sproutTaggedHeaders("2026.10.1"), `{"model":"gpt-5.4","max_tokens":9000}`)

	require.Equal(t, http.StatusNoContent, response.Code)
	require.JSONEq(t, `{"model":"gpt-5.4","max_tokens":9000}`, *body)
}

func TestSproutChildPolicyUntaggedRequestPassesThrough(t *testing.T) {
	router, body, decision := newSproutChildPolicyRouter(t, sproutChildPolicyTestConfig())

	response := doSproutChildPolicyRequest(router, "/v1/chat/completions", nil, `{"model":"gpt-5.4","max_tokens":9000}`)

	require.Equal(t, http.StatusNoContent, response.Code)
	require.JSONEq(t, `{"model":"gpt-5.4","max_tokens":9000}`, *body)
	require.Empty(t, decision.ReasonCode)
	require.Empty(t, decision.AgeBand)
}

func TestSproutChildPolicyRejectsUnknownPolicyVersion(t *testing.T) {
	router, _, decision := newSproutChildPolicyRouter(t, sproutChildPolicyTestConfig())

	response := doSproutChildPolicyRequest(router, "/v1/chat/completions", sproutTaggedHeaders("1999.1.1"), `{"model":"gpt-5-mini"}`)

	require.Equal(t, http.StatusNotFound, response.Code)
	require.Contains(t, response.Body.String(), "child safety policy")
	require.Equal(t, SproutPolicyRejectVersionUnknown, decision.ReasonCode)
}

func TestSproutChildPolicyRequiresVersionWhenConfigured(t *testing.T) {
	cfg := sproutChildPolicyTestConfig()
	cfg.Sprout.ChildPolicy.RequirePolicyVersion = true
	router, _, decision := newSproutChildPolicyRouter(t, cfg)

	response := doSproutChildPolicyRequest(router, "/v1/chat/completions", sproutTaggedHeaders(""), `{"model":"gpt-5-mini"}`)

	require.Equal(t, http.StatusNotFound, response.Code)
	require.Equal(t, SproutPolicyRejectVersionRequired, decision.ReasonCode)
}

func TestSproutChildPolicyRejectsModelOutsideAgeBand(t *testing.T) {
	router, _, decision := newSproutChildPolicyRouter(t, sproutChildPolicyTestConfig())

	response := doSproutChildPolicyRequest(router, "/v1/chat/completions", sproutTaggedHeaders("2026.10.1"), `{"model":"gpt-5.4"}`)

	require.Equal(t, http.StatusNotFound, response.Code)
	require.Equal(t, SproutPolicyRejectModelNotAllowed, decision.ReasonCode)
	require.Equal(t, "6-8", decision.AgeBand)
}

func TestSproutChildPolicyClampsAndInjectsBoundary(t *testing.T) {
	router, body, decision := newSproutChildPolicyRouter(t, sproutChildPolicyTestConfig())

	response := doSproutChildPolicyRequest(router, "/v1/chat/completions", sproutTaggedHeaders("2026.10.1"), `{"model":"gpt-5-mini","max_tokens":9000,"messages":[{"role":"user","content":"hi"}]}`)

	require.Equal(t, http.StatusNoContent, response.Code)
	require.Equal(t, "6-8", decision.AgeBand)
	require.Equal(t, 512, decision.MaxOutputTokens)

	var payload struct {
		MaxTokens int `json:"max_tokens"`
		Messages  []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	require.NoError(t, json.Unmarshal([]byte(*body), &payload))
	require.Equal(t, 512, payload.MaxTokens)
	require.Len(t, payload.Messages, 2)
	require.Equal(t, "system", payload.Messages[0].Role)
	require.Equal(t, "CHILD SAFETY BOUNDARY", payload.Messages[0].Content)
	require.Equal(t, "hi", payload.Messages[1].Content)
}

func TestSproutChildPolicyAddsCeilingWhenAbsent(t *testing.T) {
	router, body, _ := newSproutChildPolicyRouter(t, sproutChildPolicyTestConfig())

	response := doSproutChildPolicyRequest(router, "/v1/chat/completions", sproutTaggedHeaders("2026.10.2"), `{"model":"gpt-5-mini","messages":[]}`)

	require.Equal(t, http.StatusNoContent, response.Code)
	var payload map[string]any
	require.NoError(t, json.Unmarshal([]byte(*body), &payload))
	require.EqualValues(t, 256, payload["max_tokens"])
}

func TestSproutChildPolicyInjectsAnthropicSystemBoundary(t *testing.T) {
	router, body, _ := newSproutChildPolicyRouter(t, sproutChildPolicyTestConfig())

	response := doSproutChildPolicyRequest(router, "/v1/messages", sproutTaggedHeaders("2026.10.1"), `{"model":"gpt-5-mini","max_tokens":100,"system":"be nice"}`)

	require.Equal(t, http.StatusNoContent, response.Code)
	require.Contains(t, *body, "CHILD SAFETY BOUNDARY")
	require.Contains(t, *body, "be nice")
}

func TestSproutChildPolicyResponsesUsesMaxOutputTokens(t *testing.T) {
	router, body, _ := newSproutChildPolicyRouter(t, sproutChildPolicyTestConfig())

	response := doSproutChildPolicyRequest(router, "/v1/responses", sproutTaggedHeaders("2026.10.1"), `{"model":"gpt-5-mini","input":"hi"}`)

	require.Equal(t, http.StatusNoContent, response.Code)
	var payload map[string]any
	require.NoError(t, json.Unmarshal([]byte(*body), &payload))
	require.EqualValues(t, 512, payload["max_output_tokens"])
	require.Contains(t, payload["instructions"], "CHILD SAFETY BOUNDARY")
}
