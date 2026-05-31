package replication

import (
	"testing"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/validate"
)

func TestMain(m *testing.M) {
	// httptest servers bind to loopback, which SafeDialContext blocks by default.
	validate.AllowPrivateIPs.Store(true)
	m.Run()
}

const testV2Root = "/v2/"

const (
	testUser = "user"
	testPass = "pass"
)
