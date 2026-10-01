package routes

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

const sproutInternalOpenAPIPath = "sprout_internal.openapi.yaml"

func TestSproutInternalOpenAPIContract(t *testing.T) {
	specification := readSproutInternalOpenAPI(t)

	require.Equal(t, "3.0.3", specification["openapi"])
	require.Equal(t, "1.0.0", specValue(t, specification, "info", "version"))

	securityScheme := specMap(
		t,
		specValue(t, specification, "components", "securitySchemes", "serviceToken"),
	)
	require.Equal(t, "http", securityScheme["type"])
	require.Equal(t, "bearer", securityScheme["scheme"])

	runtimePath := specMap(
		t,
		specValue(t, specification, "paths", "/internal/sprout/v1/runtime"),
	)
	getOperation := specMap(t, runtimePath["get"])
	require.Equal(t, "getSproutInternalRuntime", getOperation["operationId"])
	require.Equal(
		t,
		[]any{map[string]any{"serviceToken": []any{}}},
		getOperation["security"],
	)

	responses := specMap(t, getOperation["responses"])
	require.Equal(
		t,
		"#/components/schemas/SproutRuntimeEnvelope",
		specValue(t, responses, "200", "content", "application/json", "schema", "$ref"),
	)
	require.Equal(
		t,
		"#/components/schemas/SproutErrorEnvelope",
		specValue(t, responses, "401", "content", "application/json", "schema", "$ref"),
	)

	runtimeSchema := specMap(
		t,
		specValue(t, specification, "components", "schemas", "SproutRuntimeEnvelope"),
	)
	require.Equal(t, "1.0.0", specValue(t, runtimeSchema, "properties", "schema_version", "const"))
	require.Equal(
		t,
		"#/components/schemas/SproutRuntime",
		specValue(t, runtimeSchema, "properties", "data", "$ref"),
	)

	errorSchema := specMap(
		t,
		specValue(t, specification, "components", "schemas", "SproutErrorEnvelope"),
	)
	require.Equal(t, "1.0.0", specValue(t, errorSchema, "properties", "schema_version", "const"))
	require.Equal(
		t,
		[]any{"unauthenticated"},
		specValue(t, errorSchema, "properties", "error", "properties", "code", "enum"),
	)
}

func readSproutInternalOpenAPI(t *testing.T) map[string]any {
	t.Helper()

	contents, err := os.ReadFile(sproutInternalOpenAPIPath)
	require.NoError(t, err)

	var specification map[string]any
	require.NoError(t, yaml.Unmarshal(contents, &specification))
	return specification
}

func specMap(t *testing.T, value any) map[string]any {
	t.Helper()

	result, ok := value.(map[string]any)
	require.True(t, ok, "expected mapping, got %T", value)
	return result
}

func specValue(t *testing.T, root map[string]any, path ...string) any {
	t.Helper()

	var current any = root
	for _, key := range path {
		current = specMap(t, current)[key]
		require.NotNil(t, current, "missing OpenAPI value at %v", path)
	}
	return current
}
