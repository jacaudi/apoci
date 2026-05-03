package activitypub

import (
	"context"
	"fmt"
	"sync"
)

type PackagePublisher interface {
	Publish(ctx context.Context, activityType string, object any) error
}

type FederationAdapter interface {
	PackageType() string
	APTypes() []string
	Ingest(ctx context.Context, activityType, apType string, obj map[string]any, actorURL string) error
}

type AdapterRegistry struct {
	mu    sync.RWMutex
	byAP  map[string]FederationAdapter
	byPkg map[string]FederationAdapter
}

func NewAdapterRegistry() *AdapterRegistry {
	return &AdapterRegistry{
		byAP:  map[string]FederationAdapter{},
		byPkg: map[string]FederationAdapter{},
	}
}

func (r *AdapterRegistry) Register(a FederationAdapter) error {
	if a == nil {
		return fmt.Errorf("nil adapter")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.byPkg[a.PackageType()]; ok {
		return fmt.Errorf("adapter for %q already registered", a.PackageType())
	}
	for _, t := range a.APTypes() {
		if existing, ok := r.byAP[t]; ok {
			return fmt.Errorf("AP type %q already claimed by %q", t, existing.PackageType())
		}
	}
	r.byPkg[a.PackageType()] = a
	for _, t := range a.APTypes() {
		r.byAP[t] = a
	}
	return nil
}

func (r *AdapterRegistry) Lookup(apType string) FederationAdapter {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.byAP[apType]
}
