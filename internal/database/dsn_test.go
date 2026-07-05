package database

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// See sqliteDSN in database.go for why _txlock=immediate and _busy_timeout are set.
func TestSQLiteDSNUsesImmediateTxLock(t *testing.T) {
	dsn := sqliteDSN("/var/lib/apoci/apoci.db")

	require.Contains(t, dsn, "_txlock=immediate",
		"every BEGIN must be BEGIN IMMEDIATE so _busy_timeout covers write contention")
	require.Contains(t, dsn, "_busy_timeout=10000",
		"busy_timeout must remain 10s")
}
