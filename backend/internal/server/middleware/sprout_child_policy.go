package middleware

import (
	"errors"
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/config"
	pkghttputil "github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/Wei-Shaw/sub2api/internal/pkg/requestmodel"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Sprout child-policy reason codes. They are stable, content-free, and safe to
// surface in the protocol-shaped error body and in the call audit.
const (
	SproutPolicyRejectVersionRequired  = "policy_version_required"
	SproutPolicyRejectVersionUnknown   = "policy_version_unknown"
	SproutPolicyRejectModelUnspecified = "model_unspecified"
	SproutPolicyRejectModelNotAllowed  = "model_not_allowed"
)

// sproutPolicyContextKey stores the resolved child-policy decision for the
// request so call audit and diagnostics can read it without re-resolving.
const sproutPolicyContextKey = "sprout_child_policy_decision"

// SproutPolicyDecision is the content-free result of child-policy enforcement.
// It never carries prompt or reply text.
type SproutPolicyDecision struct {
	PolicyVersion string
	AgeBand       string
	ContentRating string
	// ReasonCode is empty when the request passed; otherwise it names the
	// stable rejection reason.
	ReasonCode string
	// MaxOutputTokens is the applied ceiling (0 when not enforced).
	MaxOutputTokens int
}

// sproutCompiledPolicy is the request-time form of SproutChildPolicyConfig.
type sproutCompiledPolicy struct {
	enabled              bool
	requireVersion       bool
	defaultBand          string
	versionToBand        map[string]string
	bands                map[string]config.SproutAgeBandPolicy
	injectSafetyBoundary bool
	safetyBoundary       string
}

// SproutChildPolicy enforces the age-appropriate model and parameter policy for
// requests that carry validated Sprout device provenance.
//
// It runs after apiKeyAuth and GroupModelAllowlist and before compositeTarget so
// it sees the client-written public model name and can adjust the request body
// before routing rewrites it. Untagged callers and non-generation methods pass
// through unchanged, preserving upstream sub2api behavior.
func SproutChildPolicy(cfg *config.Config) gin.HandlerFunc {
	policy := compileSproutPolicy(cfg)
	return func(c *gin.Context) {
		if !policy.enabled || c.Request == nil || !SproutTaggedRequest(c) {
			c.Next()
			return
		}
		// Model discovery and status lookups carry no generation parameters;
		// only generation requests are gated so listing stays available.
		switch c.Request.Method {
		case http.MethodPost, http.MethodPut, http.MethodPatch:
		default:
			c.Next()
			return
		}

		labels := GetSproutRequestLabels(c)
		band, reasonCode := policy.resolveBand(labels.PolicyVersion)
		if reasonCode != "" {
			policy.reject(c, labels.PolicyVersion, config.SproutAgeBandPolicy{}, reasonCode)
			return
		}

		body, ok := readSproutPolicyBody(c)
		if !ok {
			return
		}
		candidates := requestmodel.FromBodyCandidates(c.FullPath(), c.GetHeader("Content-Type"), body)
		if len(candidates) == 0 {
			policy.reject(c, labels.PolicyVersion, band, SproutPolicyRejectModelUnspecified)
			return
		}
		for _, model := range candidates {
			if !sproutBandAllowsModel(band, model) {
				policy.reject(c, labels.PolicyVersion, band, SproutPolicyRejectModelNotAllowed)
				return
			}
		}

		rewritten := clampSproutOutputTokens(c, body, band.MaxOutputTokens)
		if policy.injectSafetyBoundary {
			rewritten = injectSproutSafetyBoundary(c, rewritten, policy.safetyBoundary)
		}
		if len(rewritten) != len(body) || string(rewritten) != string(body) {
			requestmodel.ResetRequestBody(c.Request, rewritten)
		}

		c.Set(sproutPolicyContextKey, SproutPolicyDecision{
			PolicyVersion:   labels.PolicyVersion,
			AgeBand:         band.Name,
			ContentRating:   band.ContentRating,
			MaxOutputTokens: band.MaxOutputTokens,
		})
		c.Next()
	}
}

// GetSproutPolicyDecision returns the resolved child-policy decision for the
// request. The zero value means the policy did not run.
func GetSproutPolicyDecision(c *gin.Context) SproutPolicyDecision {
	if c == nil {
		return SproutPolicyDecision{}
	}
	value, _ := c.Get(sproutPolicyContextKey)
	decision, _ := value.(SproutPolicyDecision)
	return decision
}

func compileSproutPolicy(cfg *config.Config) sproutCompiledPolicy {
	if cfg == nil || !cfg.Sprout.ChildPolicy.Enabled {
		return sproutCompiledPolicy{}
	}
	zchild := cfg.Sprout.ChildPolicy
	compiled := sproutCompiledPolicy{
		enabled:              true,
		requireVersion:       zchild.RequirePolicyVersion,
		defaultBand:          zchild.DefaultAgeBand,
		versionToBand:        make(map[string]string, len(zchild.PolicyVersions)),
		bands:                make(map[string]config.SproutAgeBandPolicy, len(zchild.AgeBands)),
		injectSafetyBoundary: zchild.InjectSafetyBoundary,
		safetyBoundary:       zchild.SafetyBoundary,
	}
	for _, band := range zchild.AgeBands {
		compiled.bands[band.Name] = band
	}
	for _, binding := range zchild.PolicyVersions {
		compiled.versionToBand[binding.Version] = binding.AgeBand
	}
	return compiled
}

func (p sproutCompiledPolicy) resolveBand(policyVersion string) (config.SproutAgeBandPolicy, string) {
	policyVersion = strings.TrimSpace(policyVersion)
	if policyVersion != "" {
		bandName, ok := p.versionToBand[policyVersion]
		if !ok {
			return config.SproutAgeBandPolicy{}, SproutPolicyRejectVersionUnknown
		}
		return p.bands[bandName], ""
	}
	if p.requireVersion {
		return config.SproutAgeBandPolicy{}, SproutPolicyRejectVersionRequired
	}
	if p.defaultBand != "" {
		return p.bands[p.defaultBand], ""
	}
	// No version and no default: fail closed rather than guess an age band.
	return config.SproutAgeBandPolicy{}, SproutPolicyRejectVersionRequired
}

func (p sproutCompiledPolicy) reject(c *gin.Context, policyVersion string, band config.SproutAgeBandPolicy, reasonCode string) {
	c.Set(sproutPolicyContextKey, SproutPolicyDecision{
		PolicyVersion:   policyVersion,
		AgeBand:         band.Name,
		ContentRating:   band.ContentRating,
		MaxOutputTokens: band.MaxOutputTokens,
		ReasonCode:      reasonCode,
	})
	service.MarkOpsClientBusinessLimited(c, service.OpsClientBusinessLimitedReasonLocalPolicyDenied)
	if reasonCode == SproutPolicyRejectModelNotAllowed || reasonCode == SproutPolicyRejectModelUnspecified {
		MarkIngressRejected(c, IngressRejectModelNotAllowed)
	}
	writeSproutPolicyRejected(c, reasonCode)
	c.Abort()
}

// writeSproutPolicyRejected returns a protocol-shaped rejection. The body
// carries only the stable reason code and a fixed message; it never echoes the
// request model, age band, or policy version.
func writeSproutPolicyRejected(c *gin.Context, reasonCode string) {
	message := "Request is not allowed by child safety policy"
	switch reasonCode {
	case SproutPolicyRejectVersionRequired:
		message = "A child safety policy version is required"
	case SproutPolicyRejectVersionUnknown:
		message = "The child safety policy version is not recognized"
	case SproutPolicyRejectModelUnspecified:
		message = "A model is required"
	case SproutPolicyRejectModelNotAllowed:
		message = "The requested model is not available for this age"
	}
	path := sproutRequestPath(c)
	switch {
	case strings.HasPrefix(path, "/v1beta") || strings.HasPrefix(path, "/antigravity/v1beta"):
		GoogleErrorWriter(c, http.StatusNotFound, message)
	case strings.Contains(path, "/messages"):
		AnthropicErrorWriter(c, http.StatusBadRequest, message)
	default:
		OpenAIErrorWriter(c, http.StatusNotFound, message)
	}
}

func sproutBandAllowsModel(band config.SproutAgeBandPolicy, model string) bool {
	model = strings.TrimSpace(model)
	for _, allowed := range band.AllowedModels {
		if allowed == model {
			return true
		}
	}
	return false
}

// readSproutPolicyBody reads and re-buffers the request body. Returning false
// means a response was already written and the caller must stop.
func readSproutPolicyBody(c *gin.Context) ([]byte, bool) {
	body, err := pkghttputil.ReadRequestBodyWithPrealloc(c.Request)
	if err != nil {
		status := http.StatusBadRequest
		message := "Failed to read request body"
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			status = http.StatusRequestEntityTooLarge
			message = "Request body is too large"
		}
		c.JSON(status, gin.H{"error": gin.H{"type": "invalid_request_error", "message": message}})
		c.Abort()
		return nil, false
	}
	requestmodel.ResetRequestBody(c.Request, body)
	return body, true
}

// sproutTokenCeilingKeys are the recognized output-token parameters across the
// supported protocols. Every present key is lowered to the band ceiling.
var sproutTokenCeilingKeys = []string{"max_tokens", "max_completion_tokens", "max_output_tokens"}

// clampSproutOutputTokens lowers every present output-token parameter to the age
// band ceiling and, when none is present, adds one so the upstream default
// cannot exceed the band. Non-JSON bodies are returned unchanged.
func clampSproutOutputTokens(c *gin.Context, body []byte, ceiling int) []byte {
	if ceiling <= 0 || !sproutLooksLikeJSON(c.GetHeader("Content-Type"), body) {
		return body
	}
	rewritten := body
	found := false
	for _, key := range sproutTokenCeilingKeys {
		value := gjson.GetBytes(rewritten, key)
		if !value.Exists() {
			continue
		}
		found = true
		if value.Type == gjson.Number && int(value.Int()) > ceiling {
			rewritten, _ = sjson.SetBytes(rewritten, key, ceiling)
		}
	}
	if found {
		return rewritten
	}
	defaultKey := "max_tokens"
	if strings.HasPrefix(sproutRequestPath(c), "/v1/responses") ||
		strings.HasPrefix(sproutRequestPath(c), "/responses") ||
		strings.HasPrefix(sproutRequestPath(c), "/backend-api/codex/responses") {
		defaultKey = "max_output_tokens"
	}
	rewritten, _ = sjson.SetBytes(rewritten, defaultKey, ceiling)
	return rewritten
}

// injectSproutSafetyBoundary prepends the mandatory child-safety boundary to the
// protocol-appropriate system field. The boundary is policy text, never user
// content, and is added idempotently: an existing boundary prefix is not
// duplicated. Unknown body shapes are left unchanged; the model and token gates
// still apply.
func injectSproutSafetyBoundary(c *gin.Context, body []byte, boundary string) []byte {
	boundary = strings.TrimSpace(boundary)
	if boundary == "" || !sproutLooksLikeJSON(c.GetHeader("Content-Type"), body) {
		return body
	}
	path := sproutRequestPath(c)
	switch {
	case strings.Contains(path, "/messages"):
		return injectAnthropicSystemBoundary(body, boundary)
	case strings.Contains(path, "/chat/completions"):
		return injectOpenAIChatBoundary(body, boundary)
	case strings.Contains(path, "/responses"):
		return injectOpenAIResponsesBoundary(body, boundary)
	case strings.Contains(path, "/models/"):
		return injectGeminiSystemBoundary(body, boundary)
	default:
		return body
	}
}

func injectAnthropicSystemBoundary(body []byte, boundary string) []byte {
	existing := gjson.GetBytes(body, "system")
	switch {
	case !existing.Exists():
		rewritten, err := sjson.SetBytes(body, "system", boundary)
		if err != nil {
			return body
		}
		return rewritten
	case existing.Type == gjson.String:
		if strings.Contains(existing.String(), boundary) {
			return body
		}
		rewritten, err := sjson.SetBytes(body, "system", boundary+"\n\n"+existing.String())
		if err != nil {
			return body
		}
		return rewritten
	case existing.IsArray():
		block, err := sjson.SetBytes([]byte(`{}`), "type", "text")
		if err != nil {
			return body
		}
		block, err = sjson.SetBytes(block, "text", boundary)
		if err != nil {
			return body
		}
		merged := "[" + string(block)
		if raw := strings.TrimSpace(existing.Raw); raw != "" && raw != "[]" {
			merged += "," + strings.Trim(raw, "[]")
		}
		merged += "]"
		rewritten, err := sjson.SetRawBytes(body, "system", []byte(merged))
		if err != nil {
			return body
		}
		return rewritten
	default:
		rewritten, err := sjson.SetBytes(body, "system", boundary)
		if err != nil {
			return body
		}
		return rewritten
	}
}

func injectOpenAIChatBoundary(body []byte, boundary string) []byte {
	messages := gjson.GetBytes(body, "messages")
	if !messages.IsArray() {
		return body
	}
	if strings.Contains(messages.Raw, boundary) {
		return body
	}
	system, err := sjson.SetBytes([]byte(`{}`), "role", "system")
	if err != nil {
		return body
	}
	system, err = sjson.SetBytes(system, "content", boundary)
	if err != nil {
		return body
	}
	merged := "[" + string(system)
	if raw := strings.TrimSpace(messages.Raw); raw != "" && raw != "[]" {
		// Append the caller messages after the injected system boundary so the
		// boundary is the first instruction the model sees.
		merged += "," + strings.Trim(raw, "[]")
	}
	merged += "]"
	rewritten, err := sjson.SetRawBytes(body, "messages", []byte(merged))
	if err != nil {
		return body
	}
	return rewritten
}

func injectOpenAIResponsesBoundary(body []byte, boundary string) []byte {
	existing := gjson.GetBytes(body, "instructions")
	if existing.Type == gjson.String {
		if strings.Contains(existing.String(), boundary) {
			return body
		}
		rewritten, err := sjson.SetBytes(body, "instructions", boundary+"\n\n"+existing.String())
		if err != nil {
			return body
		}
		return rewritten
	}
	if existing.Exists() {
		return body
	}
	rewritten, err := sjson.SetBytes(body, "instructions", boundary)
	if err != nil {
		return body
	}
	return rewritten
}

func injectGeminiSystemBoundary(body []byte, boundary string) []byte {
	existing := gjson.GetBytes(body, "systemInstruction")
	if existing.Exists() && strings.Contains(existing.Raw, boundary) {
		return body
	}
	part := []byte(`{"text":""}`)
	part, err := sjson.SetBytes(part, "text", boundary)
	if err != nil {
		return body
	}
	if existing.Exists() && existing.Get("parts").IsArray() {
		mergedParts := "[" + string(part)
		if raw := strings.TrimSpace(existing.Get("parts").Raw); raw != "" && raw != "[]" {
			mergedParts += "," + strings.Trim(raw, "[]")
		}
		mergedParts += "]"
		rewritten, err := sjson.SetRawBytes(body, "systemInstruction.parts", []byte(mergedParts))
		if err != nil {
			return body
		}
		return rewritten
	}
	rewritten, err := sjson.SetRawBytes(body, "systemInstruction", []byte(`{"parts":[`+string(part)+`]}`))
	if err != nil {
		return body
	}
	return rewritten
}

func sproutLooksLikeJSON(contentType string, body []byte) bool {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" || (trimmed[0] != '{' && trimmed[0] != '[') {
		return false
	}
	contentType = strings.ToLower(strings.TrimSpace(contentType))
	return contentType == "" || strings.Contains(contentType, "json")
}

func sproutRequestPath(c *gin.Context) string {
	if c == nil || c.Request == nil {
		return ""
	}
	if path := strings.TrimSpace(c.FullPath()); path != "" {
		return path
	}
	if c.Request.URL != nil {
		return c.Request.URL.Path
	}
	return ""
}
