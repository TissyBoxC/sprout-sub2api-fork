package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// sproutCallAuditRecorderStub is a test double for the audit write port. It records
// the exact entries the fork would persist so tests can prove no content leaks.
type sproutCallAuditRecorderStub struct {
	entries []*service.AuditLog
}

func (r *sproutCallAuditRecorderStub) Record(entry *service.AuditLog) {
	r.entries = append(r.entries, entry)
}

func newSproutCallAuditContext(t *testing.T, tagged bool) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	c.Request.RemoteAddr = "203.0.113.9:4444"
	c.Request.Header.Set("User-Agent", "sprout-test/1.0")
	c.Request.Header.Set("X-Request-Id", "req-sprout-1")
	c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{ID: 7, UserID: 42})
	if tagged {
		c.Request.Header.Set("X-Sprout-Tenant-ID", "tenant_0001")
		c.Request.Header.Set("X-Sprout-Device-ID", "device_001")
		c.Request.Header.Set("X-Sprout-Project", "voice")
		// Run the real gateway tag middleware so the context carries validated
		// labels exactly as the production route chain does.
		middleware.SproutRequestLabelsMiddleware()(c)
	}
	return c
}

func TestRecordSproutCallAuditSkipsUntaggedRequests(t *testing.T) {
	c := newSproutCallAuditContext(t, false)
	recorder := &sproutCallAuditRecorderStub{}

	recordSproutCallAudit(c, recorder, sproutCallAuditOutcome{
		Model:      "gpt-5-mini",
		Result:     "success",
		DurationMs: 12,
	})

	require.Empty(t, recorder.entries)
}

func TestRecordSproutCallAuditRecordsProvenanceWithoutContent(t *testing.T) {
	c := newSproutCallAuditContext(t, true)
	recorder := &sproutCallAuditRecorderStub{}

	recordSproutCallAudit(c, recorder, sproutCallAuditOutcome{
		Model:               "gpt-5-mini-2026",
		InputTokens:         120,
		OutputTokens:        40,
		CacheReadTokens:     10,
		CacheCreationTokens: 5,
		DurationMs:          812,
		Result:              "success",
	})

	require.Len(t, recorder.entries, 1)
	entry := recorder.entries[0]
	require.Equal(t, sproutCallAuditAction, entry.Action)
	require.Equal(t, http.MethodPost, entry.Method)
	require.Equal(t, 200, entry.StatusCode)
	require.Equal(t, int64(812), entry.LatencyMs)

	require.Equal(t, "tenant_0001", entry.Extra["tenant_id"])
	require.Equal(t, "device_001", entry.Extra["device_id"])
	require.Equal(t, "voice", entry.Extra["project"])
	require.Equal(t, "gpt-5-mini-2026", entry.Extra["model"])
	require.Equal(t, 120, entry.Extra["input_tokens"])
	require.Equal(t, 40, entry.Extra["output_tokens"])
	require.Equal(t, "success", entry.Extra["result"])
	require.NotNil(t, entry.ActorUserID)
	require.Equal(t, int64(42), *entry.ActorUserID)
	require.Equal(t, "api-key", entry.ActorEmail)

	// The audit row stores counters and provenance only: no prompt, reply, or
	// credential field may be present.
	require.NotContains(t, entry.Extra, "prompt")
	require.NotContains(t, entry.Extra, "messages")
	require.NotContains(t, entry.Extra, "content")
	require.Empty(t, entry.RequestBody)
}

func TestRecordSproutCallAuditRecordsOncePerRequest(t *testing.T) {
	c := newSproutCallAuditContext(t, true)
	recorder := &sproutCallAuditRecorderStub{}

	recordSproutCallAudit(c, recorder, sproutCallAuditOutcome{Model: "m", Result: "success"})
	// A failover that degraded after a partial result must not append a second row.
	recordSproutCallAudit(c, recorder, sproutCallAuditOutcome{Result: "failure"})

	require.Len(t, recorder.entries, 1)
}

func TestRecordSproutCallAuditOutcomeFromNilResultIsFailure(t *testing.T) {
	claude := sproutCallAuditOutcomeFromClaude(nil)
	require.Equal(t, "failure", claude.Result)
	require.Equal(t, middleware.SproutProviderUnavailableCode, claude.ReasonCode)

	openai := sproutCallAuditOutcomeFromOpenAI(nil)
	require.Equal(t, "failure", openai.Result)
	require.Equal(t, middleware.SproutProviderUnavailableCode, openai.ReasonCode)
}

func TestSproutCallAuditOutcomeFromOpenAIUsesUpstreamModel(t *testing.T) {
	outcome := sproutCallAuditOutcomeFromOpenAI(&service.OpenAIForwardResult{
		Model:         "requested-model",
		UpstreamModel: "mapped-model",
		Usage: service.OpenAIUsage{
			InputTokens:          10,
			OutputTokens:         3,
			CacheReadInputTokens: 2,
		},
	})

	require.Equal(t, "mapped-model", outcome.Model)
	require.Equal(t, "success", outcome.Result)
	require.Equal(t, 10, outcome.InputTokens)
	require.Equal(t, 3, outcome.OutputTokens)
	require.Equal(t, 2, outcome.CacheReadTokens)
}
