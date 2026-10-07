package middleware

import (
	"context"
	"net/http"
	"regexp"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

const (
	sproutTenantIDHeader       = "X-Sprout-Tenant-ID"
	sproutDeviceIDHeader       = "X-Sprout-Device-ID"
	sproutProjectHeader        = "X-Sprout-Project"
	sproutRequestPurposeHeader = "X-Sprout-Request-Purpose"
	sproutPolicyVersionHeader  = "X-Sprout-Policy-Version"
	sproutSessionIDHeader      = "X-Sprout-Session-ID"

	// SproutInvalidTagCode is returned when a caller-supplied robot device tag
	// is present but malformed. The tag identifies the origin device, so an
	// unparsable value must never be silently dropped or trusted.
	SproutInvalidTagCode = "invalid_request_tag"

	// SproutProviderUnavailableCode is the stable classification the voice
	// gateway maps to its client-facing provider_unavailable error. It is used
	// when a robot device conversation cannot be served safely.
	SproutProviderUnavailableCode = "provider_unavailable"

	// SproutProviderUnavailableMessage is deliberately generic: it carries no
	// upstream detail, prompt text, or credentials.
	SproutProviderUnavailableMessage = "AI service is temporarily unavailable"

	// maxSproutUsageCorrelationLength matches usage_logs.session_id width
	// (VARCHAR(255)). Provenance is truncated rather than aliased through a
	// database-level truncation.
	maxSproutUsageCorrelationLength = 255
)

// SproutRequestLabels contains validated pseudonymous labels for one request.
// It must never contain personal data, prompts, credentials, or free-form text.
type SproutRequestLabels struct {
	TenantID       string
	DeviceID       string
	Project        string
	RequestPurpose string
	PolicyVersion  string
	SessionID      string
}

type sproutRequestLabelsContextKey struct{}

var sproutLabelPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// Robot device identifiers must match the platform device_binding pattern
// (^[A-Za-z0-9][A-Za-z0-9._:-]{7,127}$) so an issued UUID or serial is accepted
// verbatim. The alphabet is deliberately narrow: no whitespace, '@', '/', or
// control characters, so a caller cannot smuggle free text or an email address
// into usage provenance or logs.
var sproutDeviceIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{7,127}$`)

// Project tags are bounded capability labels such as "voice"; they never carry
// user content, so the same lowercase snake_case alphabet applies.
var sproutProjectPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// SproutRequestLabelsMiddleware attaches validated X-Sprout-* labels to the
// request context and request-scoped logger.
//
// It is intentionally scoped to the AI gateway completion and model discovery
// routes (see isSproutTaggedGatewayRoute) so the headers cannot influence or
// masquerade as labels on the admin/user panel endpoints. Device and project
// headers are rejected when present but invalid; the remaining legacy labels
// keep their historical "drop invalid value" behavior for compatibility.
func SproutRequestLabelsMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request == nil || !isSproutTaggedGatewayRoute(c) {
			c.Next()
			return
		}

		deviceID, ok := validatedSproutDeviceID(c.GetHeader(sproutDeviceIDHeader))
		if !ok {
			writeSproutInvalidTag(c, sproutDeviceIDHeader)
			return
		}
		project, ok := validatedSproutProject(c.GetHeader(sproutProjectHeader))
		if !ok {
			writeSproutInvalidTag(c, sproutProjectHeader)
			return
		}

		labels := SproutRequestLabels{
			TenantID:       validSproutLabel(c.GetHeader(sproutTenantIDHeader)),
			DeviceID:       deviceID,
			Project:        project,
			RequestPurpose: validSproutLabel(c.GetHeader(sproutRequestPurposeHeader)),
			PolicyVersion:  validSproutLabel(c.GetHeader(sproutPolicyVersionHeader)),
			SessionID:      validSproutLabel(c.GetHeader(sproutSessionIDHeader)),
		}

		ctx := context.WithValue(c.Request.Context(), sproutRequestLabelsContextKey{}, labels)
		requestLogger := logger.FromContext(ctx)
		if fields := sproutLabelFields(labels); len(fields) > 0 {
			requestLogger = requestLogger.With(fields...)
			ctx = logger.IntoContext(ctx, requestLogger)
		}
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}

// sproutGatewayPathPrefixes are the AI gateway mount points. The management
// panel lives under /api/v1, which is deliberately absent so an X-Sprout-* tag
// can never be attached to an admin or user panel request.
var sproutGatewayPathPrefixes = []string{
	"/v1/",
	"/v1beta/",
	"/antigravity/",
	"/backend-api/codex/",
}

// isSproutTaggedGatewayRoute reports whether the matched route is an AI gateway
// completion or model discovery entry. Every other route (including all admin
// and user panel endpoints) ignores X-Sprout-* headers entirely.
func isSproutTaggedGatewayRoute(c *gin.Context) bool {
	if c == nil || c.Request == nil {
		return false
	}
	path := strings.TrimSpace(c.FullPath())
	if path == "" && c.Request.URL != nil {
		path = c.Request.URL.Path
	}
	if path == "" {
		return false
	}
	if !isSproutGatewayPath(path) {
		return false
	}

	switch c.Request.Method {
	case http.MethodPost:
		return strings.HasSuffix(path, "/chat/completions") ||
			strings.HasSuffix(path, "/messages") ||
			strings.HasSuffix(path, "/responses") ||
			strings.Contains(path, "/responses/") ||
			// Gemini native completion: POST /v1beta/models/{model}:{action}
			strings.Contains(path, "/models/")
	case http.MethodGet:
		// Model discovery: /v1/models, /v1/models/:model, /models, ...
		return strings.HasSuffix(path, "/models") || strings.Contains(path, "/models/")
	default:
		return false
	}
}

// isSproutGatewayPath accepts the versioned gateway prefixes plus the
// unprefixed root aliases registered for OpenAI/Responses compatibility.
func isSproutGatewayPath(path string) bool {
	for _, prefix := range sproutGatewayPathPrefixes {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	switch {
	case path == "/chat/completions":
		return true
	case path == "/responses", strings.HasPrefix(path, "/responses/"):
		return true
	case path == "/models", strings.HasPrefix(path, "/models/"):
		return true
	default:
		return false
	}
}

// GetSproutRequestLabels returns the validated labels for a request.
func GetSproutRequestLabels(c *gin.Context) SproutRequestLabels {
	if c == nil || c.Request == nil {
		return SproutRequestLabels{}
	}
	labels, _ := c.Request.Context().Value(sproutRequestLabelsContextKey{}).(SproutRequestLabels)
	return labels
}

// SproutTaggedRequest reports whether the request carries a validated robot
// device or project tag. Only satisfying requests receive the stable
// degradation error shape; untagged callers keep the existing behavior.
func SproutTaggedRequest(c *gin.Context) bool {
	labels := GetSproutRequestLabels(c)
	return labels.DeviceID != "" || labels.Project != ""
}

// WriteSproutProviderUnavailable writes the stable OpenAI-shaped degradation
// error. The voice gateway maps code=provider_unavailable to its own
// provider_unavailable client state without inspecting prompt or reply content.
func WriteSproutProviderUnavailable(c *gin.Context) {
	if c == nil {
		return
	}
	c.JSON(http.StatusServiceUnavailable, gin.H{
		"error": gin.H{
			"type":    "server_error",
			"code":    SproutProviderUnavailableCode,
			"message": SproutProviderUnavailableMessage,
		},
	})
}

// SproutUsageSessionID returns a bounded usage-log correlation value that
// carries validated device/project provenance. It never contains prompt, reply,
// credential, or audio data; unnamed requests return "" so the existing client
// session extraction applies.
//
// The fork has no dedicated provenance column and the change is scoped to
// backend/internal, so the provenance rides on the existing
// usage_logs.session_id text column.
func SproutUsageSessionID(c *gin.Context) string {
	labels := GetSproutRequestLabels(c)
	if labels.DeviceID == "" && labels.Project == "" {
		return ""
	}

	value := "sprout"
	if labels.DeviceID != "" {
		value += ";device:" + labels.DeviceID
	}
	if labels.Project != "" {
		prefix := value + ";project:"
		if remaining := maxSproutUsageCorrelationLength - len(prefix); remaining > 0 {
			project := labels.Project
			if len(project) > remaining {
				project = project[:remaining]
			}
			value += ";project:" + project
		}
	}
	if clientSessionID := service.ExtractClientSessionID(c); clientSessionID != "" {
		prefix := value + ";session:"
		if len(prefix)+len(clientSessionID) <= maxSproutUsageCorrelationLength {
			value += ";session:" + clientSessionID
		}
	}
	if len(value) > maxSproutUsageCorrelationLength {
		value = value[:maxSproutUsageCorrelationLength]
	}
	return value
}

func validSproutLabel(value string) string {
	value = strings.TrimSpace(value)
	if !sproutLabelPattern.MatchString(value) {
		return ""
	}
	return value
}

func validatedSproutDeviceID(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", true
	}
	if !sproutDeviceIDPattern.MatchString(value) {
		return "", false
	}
	return value, true
}

func validatedSproutProject(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", true
	}
	if len(value) > 128 || !sproutProjectPattern.MatchString(value) {
		return "", false
	}
	return value, true
}

func writeSproutInvalidTag(c *gin.Context, header string) {
	c.JSON(http.StatusBadRequest, gin.H{
		"error": gin.H{
			"type":    "invalid_request_error",
			"code":    SproutInvalidTagCode,
			"message": header + " is invalid",
		},
	})
	c.Abort()
}

func sproutLabelFields(labels SproutRequestLabels) []zap.Field {
	fields := make([]zap.Field, 0, 6)
	if labels.TenantID != "" {
		fields = append(fields, zap.String("tenant_id", labels.TenantID))
	}
	if labels.DeviceID != "" {
		fields = append(fields, zap.String("device_id", labels.DeviceID))
	}
	if labels.Project != "" {
		fields = append(fields, zap.String("project", labels.Project))
	}
	if labels.RequestPurpose != "" {
		fields = append(fields, zap.String("request_purpose", labels.RequestPurpose))
	}
	if labels.PolicyVersion != "" {
		fields = append(fields, zap.String("policy_version", labels.PolicyVersion))
	}
	if labels.SessionID != "" {
		fields = append(fields, zap.String("session_id", labels.SessionID))
	}
	return fields
}
