package crd_test

import (
	"testing"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/crd"
)

func TestLWSStateManager_BasicState(t *testing.T) {
	sm := crd.NewLWSStateManager()

	if sm.IsAvailable() {
		t.Error("Expected IsAvailable to be false initially")
	}

	sm.SetAvailable(true)
	if !sm.IsAvailable() {
		t.Error("Expected IsAvailable to be true after SetAvailable(true)")
	}

	sm.SetAvailable(false)
	if sm.IsAvailable() {
		t.Error("Expected IsAvailable to be false after SetAvailable(false)")
	}
}

func TestLWSStateManager_ConcurrentAccess(t *testing.T) {
	sm := crd.NewLWSStateManager()
	done := make(chan bool)

	// 10 goroutines writing
	for i := 0; i < 10; i++ {
		go func(val bool) {
			for j := 0; j < 100; j++ {
				sm.SetAvailable(val)
			}
			done <- true
		}(i%2 == 0)
	}

	// 10 goroutines reading
	for i := 0; i < 10; i++ {
		go func() {
			for j := 0; j < 100; j++ {
				_ = sm.IsAvailable()
			}
			done <- true
		}()
	}

	// Wait for all goroutines
	for i := 0; i < 20; i++ {
		<-done
	}

	// If we got here without data races, test passes
}

func TestLWSStateManager_CallbacksTriggered(t *testing.T) {
	sm := crd.NewLWSStateManager()

	callbackCalled := false
	var callbackValue bool

	sm.OnStateChange(func(available bool) {
		callbackCalled = true
		callbackValue = available
	})

	sm.SetAvailable(true)

	if !callbackCalled {
		t.Error("Expected callback to be called")
	}
	if !callbackValue {
		t.Error("Expected callback to receive true")
	}
}

func TestLWSStateManager_IdempotentStateChange(t *testing.T) {
	sm := crd.NewLWSStateManager()

	callCount := 0
	sm.OnStateChange(func(available bool) {
		callCount++
	})

	sm.SetAvailable(true)
	if callCount != 1 {
		t.Errorf("Expected 1 callback, got %d", callCount)
	}

	// Set to same value - should not trigger callback
	sm.SetAvailable(true)
	if callCount != 1 {
		t.Errorf("Expected 1 callback after idempotent change, got %d", callCount)
	}

	sm.SetAvailable(false)
	if callCount != 2 {
		t.Errorf("Expected 2 callbacks after state change, got %d", callCount)
	}
}
