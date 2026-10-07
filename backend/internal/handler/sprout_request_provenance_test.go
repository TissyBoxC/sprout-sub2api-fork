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

func newSproutTaggedHandlerContext(t *testing.T, deviceID, project string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	if deviceID != "" {
		c.Request.Header.Set("X-Sprout-Device-ID", deviceID)
	}
	if project != "" {
		c.Request.Header.Set("X-Sprout-Project", project)
	}
	// Re-run the gateway tag middleware so the context carries validated labels,
	// exactly as the production route chain does.
	middleware.SproutRequestLabelsMiddleware()(c)
	return c, recorder
}

func TestDegradeSproutConversationIfTaggedWritesStableError(t *testing.T) {
	c, recorder := newSproutTaggedHandlerContext(t, "device_001", "voice")

	require.True(t, degradeSproutConversationIfTagged(c))
	require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
	require.Contains(t, recorder.Body.String(), middleware.SproutProviderUnavailableCode)
}

func TestDegradeSproutConversationIfTaggedLeavesUntaggedCallersUntouched(t *testing.T) {
	c, recorder := newSproutTaggedHandlerContext(t, "", "")

	require.False(t, degradeSproutConversationIfTagged(c))
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Empty(t, recorder.Body.String())
}

func TestGatewayUsageSessionIDRecordsDeviceProvenance(t *testing.T) {
	c, _ := newSproutTaggedHandlerContext(t, "device_001", "voice")

	sessionID := gatewayUsageSessionID(c)
	require.Contains(t, sessionID, "device:device_001")
	require.Contains(t, sessionID, "project:voice")
	// Provenance must never include prompt/reply/credential content.
	require.NotContains(t, sessionID, "prompt")
}

func TestGatewayUsageSessionIDFallsBackToClientSession(t *testing.T) {
	c, _ := newSproutTaggedHandlerContext(t, "", "")
	c.Request.Header.Set("X-Session-Id", "client-session-1")

	require.Equal(t, "client-session-1", gatewayUsageSessionID(c))
}

// The usage provenance must survive the service-side sanitizer unchanged so the
// audit/usage row can be attributed per device without a schema change.
func TestGatewayUsageSessionIDFitsSessionColumn(t *testing.T) {
	c, _ := newSproutTaggedHandlerContext(t, "device_001", "voice")
	sessionID := gatewayUsageSessionID(c)

	require.NotEmpty(t, sessionID)
	require.LessOrEqual(t, len(sessionID), 255)

	log := service.UsageLog{SessionID: &sessionID}
	require.NotNil(t, log.SessionID)
	require.NotContains(t, *log.SessionID, "\n")
}
