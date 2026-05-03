// Package pkgfedtest holds test helpers for federation adapter tests.
package pkgfedtest

import (
	"context"
	"sync"
)

// CapturedActivity is a single Publish call recorded by StubPublisher.
type CapturedActivity struct {
	Type   string
	Object any
}

// StubPublisher implements activitypub.PackagePublisher by recording each call.
type StubPublisher struct {
	mu  sync.Mutex
	out []CapturedActivity
}

func (s *StubPublisher) Publish(_ context.Context, activityType string, object any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.out = append(s.out, CapturedActivity{Type: activityType, Object: object})
	return nil
}

func (s *StubPublisher) Activities() []CapturedActivity {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]CapturedActivity{}, s.out...)
}
