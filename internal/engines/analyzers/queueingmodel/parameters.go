package queueingmodel

import (
	"sync"
	"time"
)

// ParameterStore holds learned parameters for variants of a model/namespace.
type ParameterStore struct {
	mu     sync.RWMutex                  // needed if processing multiple variants for a model in parallel
	params map[string]*LearnedParameters // key: namespace/variantName
}

// LearnedParameters holds tuned alpha, beta, gamma for one variant
type LearnedParameters struct {
	Alpha float32
	Beta  float32
	Gamma float32

	// For continuity between tuning cycles
	NIS        float64     // Normalized Innovation Squared
	Covariance [][]float64 // state covariance matrix

	LastUpdated time.Time
}

// NewParameterStore creates a new parameter store
func NewParameterStore() *ParameterStore {
	return &ParameterStore{
		params: make(map[string]*LearnedParameters),
	}
}

// Get retrieves parameters for a variant (nil if does not exist)
func (s *ParameterStore) Get(namespace, variantName string) *LearnedParameters {
	s.mu.RLock()
	defer s.mu.RUnlock()
	key := makeVariantKey(namespace, variantName)
	return s.params[key]
}

// Set stores parameters for a variant (overrides any earlier parameters)
func (s *ParameterStore) Set(namespace, variantName string, params *LearnedParameters) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := makeVariantKey(namespace, variantName)
	s.params[key] = params
}
