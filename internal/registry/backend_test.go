package registry

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

const testNpmType = "npm"

type fakeBackend struct {
	typeName string
	prefix   string
}

func (f *fakeBackend) Type() string          { return f.typeName }
func (f *fakeBackend) RoutePrefix() string   { return f.prefix }
func (f *fakeBackend) Handler() http.Handler { return http.NotFoundHandler() }

func TestManagerRegister(t *testing.T) {
	m := NewManager()

	require.NoError(t, m.Register(&fakeBackend{typeName: testNpmType, prefix: "/npm"}))
	require.NoError(t, m.Register(&fakeBackend{typeName: "maven", prefix: "/maven"}))

	require.Len(t, m.Backends(), 2)
	require.Equal(t, testNpmType, m.Lookup(testNpmType).Type())
	require.Equal(t, "maven", m.Lookup("maven").Type())
	require.Nil(t, m.Lookup("nuget"))
}

func TestManagerRejectsDuplicateType(t *testing.T) {
	m := NewManager()
	require.NoError(t, m.Register(&fakeBackend{typeName: testNpmType, prefix: "/npm"}))
	err := m.Register(&fakeBackend{typeName: testNpmType, prefix: "/npm-alt"})
	require.Error(t, err)
}

func TestManagerRejectsDuplicatePrefix(t *testing.T) {
	m := NewManager()
	require.NoError(t, m.Register(&fakeBackend{typeName: testNpmType, prefix: "/pkg"}))
	err := m.Register(&fakeBackend{typeName: "maven", prefix: "/pkg"})
	require.Error(t, err)
}

func TestManagerRejectsBadPrefix(t *testing.T) {
	m := NewManager()

	cases := []struct {
		name   string
		prefix string
	}{
		{"empty", ""},
		{"missing slash", "npm"},
		{"root only", "/"},
		{"trailing slash", "/npm/"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := m.Register(&fakeBackend{typeName: "x", prefix: tc.prefix})
			require.Error(t, err)
		})
	}
}

func TestManagerRejectsEmptyType(t *testing.T) {
	m := NewManager()
	err := m.Register(&fakeBackend{typeName: "", prefix: "/foo"})
	require.Error(t, err)
}

func TestManagerRejectsNil(t *testing.T) {
	m := NewManager()
	require.Error(t, m.Register(nil))
}
