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

// A tagged robot conversation that exhausts quota / upstream accounts must
// return the one stable error classification the voice gateway maps to
// provider_unavailable, instead of the per-account upstream status code.
func TestOpenAIFailoverExhaustedTaggedReturnsProviderUnavailable(t *testing.T) {
	tests := []struct {
		name       string
		deviceID   string
		failover   *service.UpstreamFailoverError
		wantStatus int
		wantCode   string
	}{
		{
			name:       "tagged quota exhaustion",
			deviceID:   "device_001",
			failover:   &service.UpstreamFailoverError{StatusCode: http.StatusTooManyRequests},
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   middleware.SproutProviderUnavailableCode,
		},
		{
			name:       "untagged keeps existing mapping",
			deviceID:   "",
			failover:   &service.UpstreamFailoverError{StatusCode: http.StatusTooManyRequests},
			wantStatus: http.StatusTooManyRequests,
			wantCode:   "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			if tc.deviceID != "" {
				c.Request.Header.Set("X-Sprout-Device-ID", tc.deviceID)
			}
			middleware.SproutRequestLabelsMiddleware()(c)

			h := &OpenAIGatewayHandler{}
			h.handleFailoverExhausted(c, tc.failover, false)

			require.Equal(t, tc.wantStatus, recorder.Code)
			if tc.wantCode != "" {
				require.Contains(t, recorder.Body.String(), tc.wantCode)
				// The message must stay content-free.
				require.NotContains(t, recorder.Body.String(), "prompt")
				require.NotContains(t, recorder.Body.String(), "messages")
			}
		})
	}
}

// A stream that already committed bytes must not receive a second JSON error
// body from the degradation guard; the in-flight SSE path owns the terminal
// event. Assert the response stays in the SSE channel (event: error) rather
// than a JSON error envelope.
func TestOpenAIFailoverExhaustedTaggedStreamStartedStaysInSSEChannel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	c.Request.Header.Set("X-Sprout-Device-ID", "device_001")
	middleware.SproutRequestLabelsMiddleware()(c)

	h := &OpenAIGatewayHandler{}
	h.handleFailoverExhausted(c, &service.UpstreamFailoverError{StatusCode: http.StatusBadGateway}, true)

	body := recorder.Body.String()
	require.Contains(t, body, "event: error")
	require.NotContains(t, body, middleware.SproutProviderUnavailableCode)
}
