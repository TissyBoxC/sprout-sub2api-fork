package middleware

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func newSproutLabelsTestRouter(t *testing.T, captured **SproutRequestLabels) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(SproutRequestLabelsMiddleware())
	capture := func(c *gin.Context) {
		labels := GetSproutRequestLabels(c)
		if captured != nil {
			*captured = &labels
		}
		c.Status(http.StatusNoContent)
	}
	router.POST("/v1/chat/completions", capture)
	router.GET("/v1/models", capture)
	router.POST("/api/v1/admin/accounts", capture)
	return router
}

func doSproutRequest(router *gin.Engine, method, path string, headers map[string]string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(`{"model":"gpt-5.4"}`))
	request.Header.Set("Content-Type", "application/json")
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

func TestSproutRequestLabelsAcceptedAndRecorded(t *testing.T) {
	var labels *SproutRequestLabels
	router := newSproutLabelsTestRouter(t, &labels)

	response := doSproutRequest(router, http.MethodPost, "/v1/chat/completions", map[string]string{
		sproutDeviceIDHeader: "device_001",
		sproutProjectHeader:  "voice",
	})

	require.Equal(t, http.StatusNoContent, response.Code)
	require.NotNil(t, labels)
	require.Equal(t, "device_001", labels.DeviceID)
	require.Equal(t, "voice", labels.Project)
}

func TestSproutRequestLabelsNormalizeWhitespace(t *testing.T) {
	var labels *SproutRequestLabels
	router := newSproutLabelsTestRouter(t, &labels)

	response := doSproutRequest(router, http.MethodGet, "/v1/models", map[string]string{
		sproutDeviceIDHeader: "  device_001  ",
		sproutProjectHeader:  " voice ",
	})

	require.Equal(t, http.StatusNoContent, response.Code)
	require.Equal(t, "device_001", labels.DeviceID)
	require.Equal(t, "voice", labels.Project)
}

func TestSproutRequestLabelsRejectInvalidDeviceTags(t *testing.T) {
	long := strings.Repeat("a", 129)
	tests := []struct {
		name  string
		value string
	}{
		{name: "oversized", value: long},
		{name: "space", value: "device 001"},
		{name: "email", value: "device@example.com"},
		{name: "slash", value: "device/001"},
		{name: "control char", value: "device\ntag"},
		{name: "too short", value: "1device"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			router := newSproutLabelsTestRouter(t, nil)
			response := doSproutRequest(router, http.MethodPost, "/v1/chat/completions", map[string]string{
				sproutDeviceIDHeader: tc.value,
			})
			require.Equal(t, http.StatusBadRequest, response.Code)
			require.Contains(t, response.Body.String(), SproutInvalidTagCode)
			require.Contains(t, response.Body.String(), sproutDeviceIDHeader)
		})
	}
}

// The platform issues device identifiers in the device_binding alphabet, which
// includes uppercase, hyphens, dots, and colons. A real UUID must be accepted
// verbatim or every voice request would be rejected as an invalid tag.
func TestSproutRequestLabelsAcceptPlatformDeviceIdentifiers(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{name: "uuid", value: "72453d1e-dce3-401a-96db-e96db54c9fd5"},
		{name: "uppercase serial", value: "SPROUT-DEVICE-0001"},
		{name: "dotted serial", value: "device.001"},
		{name: "colon serial", value: "device:001"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var labels *SproutRequestLabels
			router := newSproutLabelsTestRouter(t, &labels)
			response := doSproutRequest(router, http.MethodPost, "/v1/chat/completions", map[string]string{
				sproutDeviceIDHeader: tc.value,
			})
			require.Equal(t, http.StatusNoContent, response.Code)
			require.NotNil(t, labels)
			require.Equal(t, tc.value, labels.DeviceID)
		})
	}
}

func TestSproutRequestLabelsRejectInvalidProjectTags(t *testing.T) {
	long := strings.Repeat("a", 129)
	for _, value := range []string{long, "Voice", "voice-chat", "voice chat", "voice/chat"} {
		t.Run(value, func(t *testing.T) {
			router := newSproutLabelsTestRouter(t, nil)
			response := doSproutRequest(router, http.MethodPost, "/v1/chat/completions", map[string]string{
				sproutProjectHeader: value,
			})
			require.Equal(t, http.StatusBadRequest, response.Code)
			require.Contains(t, response.Body.String(), SproutInvalidTagCode)
		})
	}
}

func TestSproutRequestLabelsUntaggedRequestsPassThrough(t *testing.T) {
	router := newSproutLabelsTestRouter(t, nil)
	response := doSproutRequest(router, http.MethodPost, "/v1/chat/completions", nil)
	require.Equal(t, http.StatusNoContent, response.Code)
}

// The tags identify robot device calls. A spoofed tag on an admin panel route
// must be ignored so it never reaches admin logs or authorization decisions.
func TestSproutRequestLabelsIgnoredOnAdminRoutes(t *testing.T) {
	var labels *SproutRequestLabels
	router := newSproutLabelsTestRouter(t, &labels)

	response := doSproutRequest(router, http.MethodPost, "/api/v1/admin/accounts", map[string]string{
		sproutDeviceIDHeader: "device_001",
		sproutProjectHeader:  "voice",
	})

	require.Equal(t, http.StatusNoContent, response.Code)
	require.NotNil(t, labels)
	require.Empty(t, labels.DeviceID)
	require.Empty(t, labels.Project)
}

func TestSproutTaggedRequestClassification(t *testing.T) {
	tests := []struct {
		name     string
		headers  map[string]string
		expected bool
	}{
		{name: "device only", headers: map[string]string{sproutDeviceIDHeader: "device_001"}, expected: true},
		{name: "project only", headers: map[string]string{sproutProjectHeader: "voice"}, expected: true},
		{name: "none", headers: nil, expected: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var tagged bool
			gin.SetMode(gin.TestMode)
			router := gin.New()
			router.Use(SproutRequestLabelsMiddleware())
			router.POST("/v1/chat/completions", func(c *gin.Context) {
				tagged = SproutTaggedRequest(c)
				c.Status(http.StatusNoContent)
			})
			doSproutRequest(router, http.MethodPost, "/v1/chat/completions", tc.headers)
			require.Equal(t, tc.expected, tagged)
		})
	}
}

func TestSproutUsageSessionIDCarriesProvenanceWithoutContent(t *testing.T) {
	tests := []struct {
		name        string
		headers     map[string]string
		expectParts []string
		expectEmpty bool
	}{
		{
			name:        "device and project",
			headers:     map[string]string{sproutDeviceIDHeader: "device_001", sproutProjectHeader: "voice"},
			expectParts: []string{"device:device_001", "project:voice"},
		},
		{
			name:        "device only",
			headers:     map[string]string{sproutDeviceIDHeader: "device_001"},
			expectParts: []string{"device:device_001"},
		},
		{
			name:        "untagged",
			headers:     nil,
			expectEmpty: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var value string
			gin.SetMode(gin.TestMode)
			router := gin.New()
			router.Use(SproutRequestLabelsMiddleware())
			router.POST("/v1/chat/completions", func(c *gin.Context) {
				value = SproutUsageSessionID(c)
				c.Status(http.StatusNoContent)
			})
			doSproutRequest(router, http.MethodPost, "/v1/chat/completions", tc.headers)

			if tc.expectEmpty {
				require.Empty(t, value)
				return
			}
			for _, part := range tc.expectParts {
				require.Contains(t, value, part)
			}
			require.LessOrEqual(t, len(value), maxSproutUsageCorrelationLength)
		})
	}
}

// Oversized provenance must be bounded, never persisted by truncation at the
// database boundary (which could alias two distinct devices).
func TestSproutUsageSessionIDIsBounded(t *testing.T) {
	deviceID := "device_" + strings.Repeat("a", 100)
	var value string
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(SproutRequestLabelsMiddleware())
	router.POST("/v1/chat/completions", func(c *gin.Context) {
		value = SproutUsageSessionID(c)
		c.Status(http.StatusNoContent)
	})
	doSproutRequest(router, http.MethodPost, "/v1/chat/completions", map[string]string{
		sproutDeviceIDHeader:  deviceID,
		sproutProjectHeader:   "voice",
		sproutSessionIDHeader: strings.Repeat("s", 200),
	})
	require.LessOrEqual(t, len(value), maxSproutUsageCorrelationLength)
}

func TestWriteSproutProviderUnavailableShape(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	WriteSproutProviderUnavailable(c)

	require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
	var payload struct {
		Error struct {
			Type    string `json:"type"`
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &payload))
	require.Equal(t, SproutProviderUnavailableCode, payload.Error.Code)
	require.Equal(t, "server_error", payload.Error.Type)
	require.NotEmpty(t, payload.Error.Message)
}
