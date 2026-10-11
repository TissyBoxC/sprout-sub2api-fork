package handler

import (
	"strings"
	"time"

	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// sproutCallAuditAction is the stable audit action for one Sprout model call.
const sproutCallAuditAction = "sprout.model_call"

// sproutCallAuditRecordedKey guards against recording more than one call-audit
// row for a request. A failover attempt that produced a partial result and then
// degraded would otherwise write both a success and a failure row.
const sproutCallAuditRecordedKey = "sprout_call_audit_recorded"

// sproutCallAuditServiceKey binds the request-scoped audit service so shared
// helpers such as degradeSproutConversationIfTagged can write a failure audit
// without every call site threading the service through.
const sproutCallAuditServiceKey = "sprout_call_audit_service"

// sproutCallAuditRecorder is the narrow audit surface the fork needs. It is
// satisfied by *service.AuditLogService and lets the call-audit path be tested
// without a live database writer.
type sproutCallAuditRecorder interface {
	Record(entry *service.AuditLog)
}

// bindSproutCallAudit attaches the handler's audit service to the request. It is
// safe to call once at the top of a gateway handler.
func bindSproutCallAudit(c *gin.Context, auditService *service.AuditLogService) {
	if c == nil || auditService == nil {
		return
	}
	c.Set(sproutCallAuditServiceKey, auditService)
}

func sproutCallAuditServiceFromContext(c *gin.Context) sproutCallAuditRecorder {
	if c == nil {
		return nil
	}
	value, ok := c.Get(sproutCallAuditServiceKey)
	if !ok {
		return nil
	}
	auditService, _ := value.(sproutCallAuditRecorder)
	return auditService
}

// sproutCallAuditOutcome carries the content-free per-call statistics known once
// a model call finishes. It holds counts and status only: no prompt, reply,
// audio, or credential ever enters this struct.
type sproutCallAuditOutcome struct {
	Model               string
	InputTokens         int
	OutputTokens        int
	CacheReadTokens     int
	CacheCreationTokens int
	DurationMs          int64
	// Result is a stable classification: success, failure, or the degradation
	// reason (for example provider_unavailable). ReasonCode carries the child
	// policy reason when the policy rejected the call.
	Result     string
	ReasonCode string
}

// recordSproutCallAudit writes one sanitized model-call audit row for a
// Sprout-tagged request. Untagged callers and a missing audit service are
// no-ops so upstream behavior is unchanged.
//
// The recorded fields are limited to validated pseudonymous provenance, model
// name, token counts, latency, result status, policy version, age band, and
// reason code. Prompt and response content is never read, so it can never be
// stored.
func recordSproutCallAudit(
	c *gin.Context,
	auditService sproutCallAuditRecorder,
	outcome sproutCallAuditOutcome,
) {
	if c == nil || auditService == nil || !middleware2.SproutTaggedRequest(c) {
		return
	}
	if alreadyRecorded, ok := c.Get(sproutCallAuditRecordedKey); ok {
		if recorded, _ := alreadyRecorded.(bool); recorded {
			return
		}
	}
	c.Set(sproutCallAuditRecordedKey, true)

	labels := middleware2.GetSproutRequestLabels(c)
	decision := middleware2.GetSproutPolicyDecision(c)

	extra := map[string]any{
		"result":         sproutCallAuditResult(outcome.Result),
		"model":          strings.TrimSpace(outcome.Model),
		"input_tokens":   outcome.InputTokens,
		"output_tokens":  outcome.OutputTokens,
		"duration_ms":    outcome.DurationMs,
		"policy_version": decision.PolicyVersion,
	}
	if labels.TenantID != "" {
		extra["tenant_id"] = labels.TenantID
	}
	if labels.DeviceID != "" {
		extra["device_id"] = labels.DeviceID
	}
	if labels.Project != "" {
		extra["project"] = labels.Project
	}
	if outcome.CacheReadTokens > 0 {
		extra["cache_read_tokens"] = outcome.CacheReadTokens
	}
	if outcome.CacheCreationTokens > 0 {
		extra["cache_creation_tokens"] = outcome.CacheCreationTokens
	}
	if decision.AgeBand != "" {
		extra["age_band"] = decision.AgeBand
	}
	if decision.ContentRating != "" {
		extra["content_rating"] = decision.ContentRating
	}
	reasonCode := strings.TrimSpace(outcome.ReasonCode)
	if reasonCode == "" {
		reasonCode = strings.TrimSpace(decision.ReasonCode)
	}
	if reasonCode != "" {
		extra["reason_code"] = reasonCode
	}

	entry := &service.AuditLog{
		CreatedAt:        time.Now().UTC(),
		AuthMethod:       "api_key",
		CredentialMasked: middleware2.MaskedRequestCredential(c),
		Action:           sproutCallAuditAction,
		Method:           sproutCallAuditMethod(c),
		Path:             sproutCallAuditPath(c),
		StatusCode:       sproutCallAuditStatusCode(outcome.Result),
		LatencyMs:        outcome.DurationMs,
		Extra:            extra,
	}
	if request := c.Request; request != nil {
		entry.UserAgent = request.UserAgent()
		entry.ClientIP = c.ClientIP()
		if request.URL != nil {
			entry.RequestID = strings.TrimSpace(c.GetHeader("X-Request-Id"))
		}
	}
	// Attribute the call to the masked credential and owning user without
	// storing the key material itself.
	if apiKey, ok := middleware2.GetAPIKeyFromContext(c); ok && apiKey != nil {
		entry.ActorEmail = "api-key"
		if apiKey.UserID > 0 {
			entry.ActorUserID = &apiKey.UserID
		}
	}
	auditService.Record(entry)
}

// recordSproutCallAuditFailure writes a sanitized failure audit row for a
// Sprout-tagged request that never produced an upstream result. It is a no-op
// when a call audit was already recorded for the request.
func recordSproutCallAuditFailure(c *gin.Context, auditService sproutCallAuditRecorder) {
	recordSproutCallAudit(c, auditService, sproutCallAuditOutcome{
		Result:     "failure",
		ReasonCode: middleware2.SproutProviderUnavailableCode,
	})
}

// recordSproutCallAuditOutcome records a completed Sprout call when the request
// carries a bound audit service. It is the single entry point used by the
// gateway handlers after a model call finishes (success or failure).
func recordSproutCallAuditOutcome(c *gin.Context, outcome sproutCallAuditOutcome) {
	recordSproutCallAudit(c, sproutCallAuditServiceFromContext(c), outcome)
}

func sproutCallAuditResult(result string) string {
	switch strings.ToLower(strings.TrimSpace(result)) {
	case "success":
		return "success"
	case "", "failure":
		return "failure"
	default:
		return strings.ToLower(strings.TrimSpace(result))
	}
}

func sproutCallAuditStatusCode(result string) int {
	if sproutCallAuditResult(result) == "success" {
		return 200
	}
	return 502
}

func sproutCallAuditMethod(c *gin.Context) string {
	if c == nil || c.Request == nil {
		return ""
	}
	return c.Request.Method
}

func sproutCallAuditPath(c *gin.Context) string {
	if c == nil {
		return ""
	}
	if path := strings.TrimSpace(c.FullPath()); path != "" {
		return path
	}
	if c.Request != nil && c.Request.URL != nil {
		return c.Request.URL.Path
	}
	return ""
}

// sproutCallAuditOutcomeFromClaude builds the audit statistics for a completed
// Anthropic-protocol call. A nil result means the call failed before an upstream
// result was available.
func sproutCallAuditOutcomeFromClaude(result *service.ForwardResult) sproutCallAuditOutcome {
	if result == nil {
		return sproutCallAuditOutcome{
			Result:     "failure",
			ReasonCode: middleware2.SproutProviderUnavailableCode,
		}
	}
	outcome := sproutCallAuditOutcome{
		Model:               result.UpstreamModel,
		InputTokens:         result.Usage.InputTokens,
		OutputTokens:        result.Usage.OutputTokens,
		CacheReadTokens:     result.Usage.CacheReadInputTokens,
		CacheCreationTokens: result.Usage.CacheCreationInputTokens,
		DurationMs:          result.Duration.Milliseconds(),
		Result:              "success",
	}
	if outcome.Model == "" {
		outcome.Model = result.Model
	}
	// A committed stream the client abandoned still produced upstream usage; it
	// is not a policy or provider failure, so it keeps the success result.
	return outcome
}

// sproutCallAuditOutcomeFromOpenAI builds the audit statistics for a completed
// OpenAI-protocol call. A nil result means the call failed before an upstream
// result was available.
func sproutCallAuditOutcomeFromOpenAI(result *service.OpenAIForwardResult) sproutCallAuditOutcome {
	if result == nil {
		return sproutCallAuditOutcome{
			Result:     "failure",
			ReasonCode: middleware2.SproutProviderUnavailableCode,
		}
	}
	outcome := sproutCallAuditOutcome{
		Model:               result.UpstreamModel,
		InputTokens:         result.Usage.InputTokens,
		OutputTokens:        result.Usage.OutputTokens,
		CacheReadTokens:     result.Usage.CacheReadInputTokens,
		CacheCreationTokens: result.Usage.CacheCreationInputTokens,
		DurationMs:          result.Duration.Milliseconds(),
		Result:              "success",
	}
	if outcome.Model == "" {
		outcome.Model = result.Model
	}
	return outcome
}
