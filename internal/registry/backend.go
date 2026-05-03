package registry

import (
	"fmt"
	"net/http"
	"strings"
)

type Backend interface {
	Type() string
	RoutePrefix() string
	Handler() http.Handler
}

type Manager struct {
	backends []Backend
}

func NewManager() *Manager {
	return &Manager{}
}

func (m *Manager) Register(b Backend) error {
	if b == nil {
		return fmt.Errorf("registry: nil backend")
	}
	if b.Type() == "" {
		return fmt.Errorf("registry: backend type is empty")
	}
	prefix := b.RoutePrefix()
	if !strings.HasPrefix(prefix, "/") || prefix == "/" {
		return fmt.Errorf("registry: backend %q route prefix %q must start with %q and not be %q", b.Type(), prefix, "/", "/")
	}
	if strings.HasSuffix(prefix, "/") {
		return fmt.Errorf("registry: backend %q route prefix %q must not end with %q", b.Type(), prefix, "/")
	}

	for _, existing := range m.backends {
		if existing.Type() == b.Type() {
			return fmt.Errorf("registry: backend type %q already registered", b.Type())
		}
		if existing.RoutePrefix() == prefix {
			return fmt.Errorf("registry: route prefix %q already registered by %q", prefix, existing.Type())
		}
	}
	m.backends = append(m.backends, b)
	return nil
}

func (m *Manager) Backends() []Backend {
	out := make([]Backend, len(m.backends))
	copy(out, m.backends)
	return out
}

func (m *Manager) Lookup(pkgType string) Backend {
	for _, b := range m.backends {
		if b.Type() == pkgType {
			return b
		}
	}
	return nil
}
