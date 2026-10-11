package handler

import (
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// gatewayUsageSessionID resolves the bounded usage-log correlation value for one
// gateway request. When the request carries validated Sprout robot provenance
// (device/project tags) it records that provenance into the existing
// usage_logs.session_id column; otherwise it preserves the historical explicit
// client session identifier.
//
// The value never contains prompt, reply, credential, or audio content, and the
// fork keeps a single bounded column instead of adding a schema migration.
func gatewayUsageSessionID(c *gin.Context) string {
	if provenance := middleware2.SproutUsageSessionID(c); provenance != "" {
		return provenance
	}
	return service.ExtractClientSessionID(c)
}

// degradeSproutConversationIfTagged converts a scheduler, quota, or upstream
// failure on a robot device conversation into the stable provider_unavailable
// error the voice gateway maps to its own provider_unavailable state. It
// reports whether it wrote a response so untagged callers keep their existing
// error shapes.
//
// It must only run before any response bytes are committed; a stream that has
// already started reports its own terminal event.
func degradeSproutConversationIfTagged(c *gin.Context) bool {
	if !middleware2.SproutTaggedRequest(c) {
		return false
	}
	// Never append a JSON error body to a stream that already committed bytes;
	// the in-flight SSE path owns the terminal event.
	if c.Writer.Written() || service.IsResponseCommitted(c) {
		return false
	}
	// The degradation is the terminal result for a robot conversation that never
	// produced an upstream result, so record the sanitized failure call audit
	// here (the success path records its own row). Untagged callers never reach
	// this point.
	recordSproutCallAuditFailure(c, sproutCallAuditServiceFromContext(c))
	middleware2.WriteSproutProviderUnavailable(c)
	return true
}
