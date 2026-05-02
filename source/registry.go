package source

import (
	"errors"
	"fmt"
	"slices"
	"sync"
)

// ErrAlreadyRegistered is returned by Register when a name is already in use.
var ErrAlreadyRegistered = errors.New("source: name already registered")

// Factory is a constructor function that returns a new Source instance.
type Factory func() (Source, error)

// Registry maps source names to their Factory functions.
// It is safe for concurrent use.
type Registry struct {
	mu        sync.RWMutex
	factories map[string]Factory
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{
		factories: make(map[string]Factory),
	}
}

// Register associates name with f. It returns ErrAlreadyRegistered if name
// is already present.
func (r *Registry) Register(name string, f Factory) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.factories[name]; exists {
		return fmt.Errorf("%w: %q", ErrAlreadyRegistered, name)
	}
	r.factories[name] = f
	return nil
}

// Get returns the Factory for name and true, or nil and false if not found.
func (r *Registry) Get(name string) (Factory, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	f, ok := r.factories[name]
	return f, ok
}

// Names returns the sorted list of registered source names.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	names := make([]string, 0, len(r.factories))
	for n := range r.factories {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}
