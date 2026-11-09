package connectorbuild

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/connectors"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// Deps are the host services a Factory may use. Factories must not read the
// process environment or global state; everything comes from the Connection
// and Deps.
type Deps struct {
	// Credentials resolves conn/<id>/<field> secrets.
	Credentials ports.CredentialProvider
	// HTTPClient returns the client for provider calls; nil selects the
	// adapter default (no redirects, bounded responses).
	HTTPClient func() *http.Client
	// Now is the clock; nil selects time.Now.
	Now func() time.Time

	// Legacy-only inputs, used by factories that wrap a ports.Connector
	// until the provider is ported (WP0.6).

	// Secrets is the raw secret store with legacy key names (github/token).
	Secrets ports.SecretStore
	// Sink receives legacy Markdown snapshots.
	Sink connectors.MarkdownSink
	// StatePath is the legacy IMAP cursor file; empty keeps IMAP stateless.
	StatePath string
}

// Factory builds a Source for one connection. The registry has already run
// SourceDescriptor.ValidateConnection on conn.
type Factory func(conn domain.Connection, deps Deps) (ports.Source, error)

// Registration is one provider entry.
type Registration struct {
	Descriptor domain.SourceDescriptor
	Factory    Factory
}

// Registry maps provider names to descriptors and factories. It is safe for
// concurrent use. Composition roots build one with Builtin (or NewRegistry
// plus Register) and pass it down; there is no package-level mutable state.
type Registry struct {
	mu      sync.RWMutex
	entries map[string]Registration
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{entries: map[string]Registration{}}
}

// Register adds a provider. Invalid descriptors and nil factories return a
// validation error; an already registered provider returns domain.ErrConflict.
func (r *Registry) Register(descriptor domain.SourceDescriptor, factory Factory) error {
	if err := descriptor.Validate(); err != nil {
		return err
	}
	if factory == nil {
		return domain.NewValidationError("registry.factory", "is required for "+descriptor.Provider)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.entries[descriptor.Provider]; exists {
		return fmt.Errorf("%w: provider %q is already registered", domain.ErrConflict, descriptor.Provider)
	}
	r.entries[descriptor.Provider] = Registration{Descriptor: cloneDescriptor(descriptor), Factory: factory}
	return nil
}

// Lookup returns the provider's registration or domain.ErrNotFound.
func (r *Registry) Lookup(provider string) (Registration, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	entry, ok := r.entries[provider]
	if !ok {
		return Registration{}, fmt.Errorf("%w: unknown provider %q", domain.ErrNotFound, provider)
	}
	entry.Descriptor = cloneDescriptor(entry.Descriptor)
	return entry, nil
}

// Descriptors returns copies of every descriptor sorted by provider.
func (r *Registry) Descriptors() []domain.SourceDescriptor {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]domain.SourceDescriptor, 0, len(r.entries))
	for _, entry := range r.entries {
		out = append(out, cloneDescriptor(entry.Descriptor))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Provider < out[j].Provider })
	return out
}

// Providers returns the registered provider names sorted; use it instead of
// hard-coded name lists in CLI help, completion and API enums.
func (r *Registry) Providers() []string {
	descriptors := r.Descriptors()
	names := make([]string, len(descriptors))
	for index, descriptor := range descriptors {
		names[index] = descriptor.Provider
	}
	return names
}

// Build validates conn against its provider's descriptor and runs the
// factory. The returned source must report the same provider.
func (r *Registry) Build(conn domain.Connection, deps Deps) (ports.Source, error) {
	entry, err := r.Lookup(conn.Provider)
	if err != nil {
		return nil, err
	}
	if err := entry.Descriptor.ValidateConnection(conn); err != nil {
		return nil, err
	}
	source, err := entry.Factory(conn, deps)
	if err != nil {
		return nil, err
	}
	if source == nil {
		return nil, fmt.Errorf("connectorbuild: factory for %q returned no source", conn.Provider)
	}
	if got := source.Descriptor().Provider; got != conn.Provider {
		return nil, fmt.Errorf("connectorbuild: factory for %q built a %q source", conn.Provider, got)
	}
	return source, nil
}

// SecretCredentials is the CredentialProvider over a SecretStore using the
// conn/<id>/<field> namespace. It adds naming only, not storage.
type SecretCredentials struct {
	Store ports.SecretStore
}

// Get resolves domain.CredentialKey(connectionID, field).
func (s SecretCredentials) Get(ctx context.Context, connectionID, field string) (string, error) {
	if s.Store == nil {
		return "", domain.NewValidationError("credentials.store", "is required")
	}
	key, err := domain.CredentialKey(connectionID, field)
	if err != nil {
		return "", err
	}
	return s.Store.Get(ctx, key)
}

func cloneDescriptor(descriptor domain.SourceDescriptor) domain.SourceDescriptor {
	descriptor.Capabilities.AuthModes = slices.Clone(descriptor.Capabilities.AuthModes)
	descriptor.ConfigFields = slices.Clone(descriptor.ConfigFields)
	descriptor.Backends = slices.Clone(descriptor.Backends)
	descriptor.Streams = slices.Clone(descriptor.Streams)
	return descriptor
}
