package controller_test

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/controller"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/crd"
)

func TestCRDWatcher_Creation(t *testing.T) {
	fakeClient := fake.NewClientBuilder().Build()
	stateManager := crd.NewLWSStateManager()
	logger := logr.Discard()

	watcher := controller.NewCRDWatcher(fakeClient, "leaderworkersets.leaderworkerset.x-k8s.io", stateManager, logger)
	if watcher == nil {
		t.Error("Expected NewCRDWatcher to return non-nil watcher")
	}
}

func TestCRDWatcher_DetectsCRDCreate(t *testing.T) {
	lwsCRD := &apiextensionsv1.CustomResourceDefinition{}
	lwsCRD.Name = "leaderworkersets.leaderworkerset.x-k8s.io"

	scheme := runtime.NewScheme()
	_ = apiextensionsv1.AddToScheme(scheme)

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(lwsCRD).
		Build()

	stateManager := crd.NewLWSStateManager()
	logger := logr.Discard()

	watcher := controller.NewCRDWatcher(fakeClient, "leaderworkersets.leaderworkerset.x-k8s.io", stateManager, logger)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go func() {
		_ = watcher.Start(ctx)
	}()

	// Give watcher time to detect CRD
	time.Sleep(100 * time.Millisecond)

	if !stateManager.IsAvailable() {
		t.Error("Expected state manager to show CRD as available")
	}
}

func TestCRDWatcher_DetectsCRDDelete(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = apiextensionsv1.AddToScheme(scheme)

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		Build()

	stateManager := crd.NewLWSStateManager()
	stateManager.SetAvailable(true) // Start with CRD available

	logger := logr.Discard()

	watcher := controller.NewCRDWatcher(fakeClient, "leaderworkersets.leaderworkerset.x-k8s.io", stateManager, logger)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go func() {
		_ = watcher.Start(ctx)
	}()

	// Give watcher time to detect CRD absence
	time.Sleep(100 * time.Millisecond)

	if stateManager.IsAvailable() {
		t.Error("Expected state manager to show CRD as unavailable")
	}
}
