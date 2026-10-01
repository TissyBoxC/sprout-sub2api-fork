package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestSproutRequestLabelsMiddlewarePreservesValidLabels(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(SproutRequestLabelsMiddleware())
	router.GET("/", func(c *gin.Context) {
		labels := GetSproutRequestLabels(c)
		require.Equal(t, "tenant_001", labels.TenantID)
		require.Equal(t, "device_001", labels.DeviceID)
		require.Equal(t, "voice_conversation", labels.RequestPurpose)
		c.Status(http.StatusNoContent)
	})

	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set(sproutTenantIDHeader, "tenant_001")
	request.Header.Set(sproutDeviceIDHeader, " device_001 ")
	request.Header.Set(sproutRequestPurposeHeader, "voice_conversation")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	require.Equal(t, http.StatusNoContent, response.Code)
}

func TestSproutRequestLabelsMiddlewareDropsUnsafeLabels(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(SproutRequestLabelsMiddleware())
	router.GET("/", func(c *gin.Context) {
		labels := GetSproutRequestLabels(c)
		require.Empty(t, labels.TenantID)
		require.Empty(t, labels.DeviceID)
		require.Empty(t, labels.SessionID)
		c.Status(http.StatusNoContent)
	})

	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set(sproutTenantIDHeader, "tenant id")
	request.Header.Set(sproutDeviceIDHeader, "device@example.com")
	request.Header.Set(sproutSessionIDHeader, "session\nforged")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	require.Equal(t, http.StatusNoContent, response.Code)
}
