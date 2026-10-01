package middleware

import (
	"context"
	"regexp"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

const (
	sproutTenantIDHeader       = "X-Sprout-Tenant-ID"
	sproutDeviceIDHeader       = "X-Sprout-Device-ID"
	sproutRequestPurposeHeader = "X-Sprout-Request-Purpose"
	sproutPolicyVersionHeader  = "X-Sprout-Policy-Version"
	sproutSessionIDHeader      = "X-Sprout-Session-ID"
)

// SproutRequestLabels contains validated pseudonymous labels for one request.
// It must never contain personal data, prompts, credentials, or free-form text.
type SproutRequestLabels struct {
	TenantID       string
	DeviceID       string
	RequestPurpose string
	PolicyVersion  string
	SessionID      string
}

type sproutRequestLabelsContextKey struct{}

var sproutLabelPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// SproutRequestLabelsMiddleware attaches validated X-Sprout-* labels to the
// request context and request-scoped logger. Invalid values are discarded.
func SproutRequestLabelsMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request == nil {
			c.Next()
			return
		}

		labels := SproutRequestLabels{
			TenantID:       validSproutLabel(c.GetHeader(sproutTenantIDHeader)),
			DeviceID:       validSproutLabel(c.GetHeader(sproutDeviceIDHeader)),
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

// GetSproutRequestLabels returns the validated labels for a request.
func GetSproutRequestLabels(c *gin.Context) SproutRequestLabels {
	if c == nil || c.Request == nil {
		return SproutRequestLabels{}
	}
	labels, _ := c.Request.Context().Value(sproutRequestLabelsContextKey{}).(SproutRequestLabels)
	return labels
}

func validSproutLabel(value string) string {
	value = strings.TrimSpace(value)
	if !sproutLabelPattern.MatchString(value) {
		return ""
	}
	return value
}

func sproutLabelFields(labels SproutRequestLabels) []zap.Field {
	fields := make([]zap.Field, 0, 5)
	if labels.TenantID != "" {
		fields = append(fields, zap.String("tenant_id", labels.TenantID))
	}
	if labels.DeviceID != "" {
		fields = append(fields, zap.String("device_id", labels.DeviceID))
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
