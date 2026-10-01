package routes

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

func TestSproutInternalRoutesDisabledByDefault(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	RegisterSproutInternalRoutes(router, &config.Config{})

	response := httptest.NewRecorder()
	router.ServeHTTP(
		response,
		httptest.NewRequest(http.MethodGet, "/internal/sprout/v1/runtime", nil),
	)

	require.Equal(t, http.StatusNotFound, response.Code)
}

func TestSproutInternalRuntimeRequiresServiceToken(t *testing.T) {
	tests := []struct {
		name   string
		header string
	}{
		{name: "missing token"},
		{name: "wrong token", header: "Bearer " + strings.Repeat("y", 32)},
		{name: "wrong scheme", header: "Basic " + strings.Repeat("x", 32)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			router := newSproutInternalTestRouter()
			request := httptest.NewRequest(
				http.MethodGet,
				"/internal/sprout/v1/runtime",
				nil,
			)
			if tt.header != "" {
				request.Header.Set("Authorization", tt.header)
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)

			require.Equal(t, http.StatusUnauthorized, response.Code)
			require.NotContains(t, response.Body.String(), strings.Repeat("x", 32))

			var envelope sproutResponseEnvelope
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &envelope))
			require.Equal(t, sproutResponseSchemaVersion, envelope.SchemaVersion)
			require.NotEmpty(t, envelope.RequestID)
			require.NotNil(t, envelope.Error)
			require.Equal(t, "unauthenticated", envelope.Error.Code)
		})
	}
}

func TestSproutInternalRuntimeReturnsSafeEnvelope(t *testing.T) {
	router := newSproutInternalTestRouter()
	request := httptest.NewRequest(
		http.MethodGet,
		"/internal/sprout/v1/runtime",
		nil,
	)
	request.Header.Set("Authorization", "Bearer "+strings.Repeat("x", 32))
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	require.Equal(t, http.StatusOK, response.Code)

	var envelope struct {
		SchemaVersion string           `json:"schema_version"`
		RequestID     string           `json:"request_id"`
		Data          map[string]any   `json:"data"`
		Error         *sproutErrorBody `json:"error"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &envelope))
	require.Equal(t, sproutResponseSchemaVersion, envelope.SchemaVersion)
	require.NotEmpty(t, envelope.RequestID)
	require.Nil(t, envelope.Error)
	require.Equal(t, "sprout-sub2api", envelope.Data["service"])
	require.Equal(t, "running", envelope.Data["status"])
	require.NotContains(t, response.Body.String(), strings.Repeat("x", 32))
}

func newSproutInternalTestRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	RegisterSproutInternalRoutes(router, &config.Config{
		Sprout: config.SproutConfig{
			InternalAPI: config.SproutInternalAPIConfig{
				Enabled: true,
				Token:   strings.Repeat("x", 32),
			},
		},
	})
	return router
}
