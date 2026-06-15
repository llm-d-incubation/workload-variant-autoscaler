package crd

import "sync"

// CRDStateManager is an interface for tracking CRD availability state.
// Implemented by LWSStateManager and can be implemented by other CRD-specific managers.
type CRDStateManager interface {
	IsAvailable() bool
	SetAvailable(available bool)
}

// LWSStateManager tracks LeaderWorkerSet CRD availability in a thread-safe manner.
type LWSStateManager struct {
	mu        sync.RWMutex
	available bool
	callbacks []func(bool)
}

// NewLWSStateManager creates a new LWSStateManager with available set to false.
func NewLWSStateManager() *LWSStateManager {
	return &LWSStateManager{
		available: false,
		callbacks: []func(bool){},
	}
}

// IsAvailable returns whether the LeaderWorkerSet CRD is currently available.
func (m *LWSStateManager) IsAvailable() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.available
}

// SetAvailable updates the availability state of the LeaderWorkerSet CRD.
// If the state changes, all registered callbacks are invoked with the new state.
func (m *LWSStateManager) SetAvailable(available bool) {
	m.mu.Lock()
	oldValue := m.available
	m.available = available
	callbacks := m.callbacks
	m.mu.Unlock()

	// Only trigger callbacks if state actually changed
	if oldValue != available {
		for _, cb := range callbacks {
			cb(available)
		}
	}
}

// OnStateChange registers a callback to be invoked when the availability state changes.
func (m *LWSStateManager) OnStateChange(callback func(bool)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.callbacks = append(m.callbacks, callback)
}
