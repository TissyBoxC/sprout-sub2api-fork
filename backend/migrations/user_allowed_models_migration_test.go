package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUserAllowedModelsMigration(t *testing.T) {
	content, err := FS.ReadFile("241_user_allowed_models.sql")
	require.NoError(t, err)

	sql := strings.Join(strings.Fields(string(content)), " ")
	require.Contains(t, sql, "ALTER TABLE users")
	require.Contains(t, sql, "ADD COLUMN IF NOT EXISTS allowed_models JSONB NOT NULL DEFAULT '[]'::jsonb")
	require.Contains(t, sql, "COMMENT ON COLUMN users.allowed_models")
}
