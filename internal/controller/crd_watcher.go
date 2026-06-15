package controller

import (
	"context"
	"time"

	"github.com/go-logr/logr"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/crd"
)

// CRDWatcher watches CustomResourceDefinition resources and updates a state manager
// when the CRD is created or deleted. It is generic and can be used for any CRD.
type CRDWatcher struct {
	client.Client
	crdName      string
	stateManager crd.CRDStateManager
	logger       logr.Logger
}

// NewCRDWatcher creates a new CRDWatcher for the specified CRD.
func NewCRDWatcher(c client.Client, crdName string, stateManager crd.CRDStateManager, logger logr.Logger) *CRDWatcher {
	return &CRDWatcher{
		Client:       c,
		crdName:      crdName,
		stateManager: stateManager,
		logger:       logger,
	}
}

// Start implements manager.Runnable.
func (w *CRDWatcher) Start(ctx context.Context) error {
	return w.watchWithRetry(ctx)
}

// watchWithRetry starts the CRD watch with exponential backoff retry on failures.
func (w *CRDWatcher) watchWithRetry(ctx context.Context) error {
	backoff := constants.CRDWatchBackoff

	return wait.ExponentialBackoffWithContext(ctx, backoff, func(ctx context.Context) (bool, error) {
		err := w.watch(ctx)
		if err == nil || ctx.Err() != nil {
			return true, err
		}

		w.logger.Error(err, "CRD watch failed, will retry",
			"backoff", backoff.Duration)
		return false, nil
	})
}

// watch performs the actual watch on CRD resources.
func (w *CRDWatcher) watch(ctx context.Context) error {
	// Initial check for CRD existence
	crdObj := &apiextensionsv1.CustomResourceDefinition{}
	err := w.Get(ctx, client.ObjectKey{Name: w.crdName}, crdObj)
	if err == nil {
		w.stateManager.SetAvailable(true)
		w.logger.Info("CRD detected during initial check", "crd", w.crdName)
	} else {
		w.stateManager.SetAvailable(false)
	}

	// Watch for CRD changes
	// For now, we poll every 30 seconds as a simple implementation
	// This can be enhanced to use actual watches in the future
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			err := w.Get(ctx, client.ObjectKey{Name: w.crdName}, crdObj)
			if err == nil {
				if !w.stateManager.IsAvailable() {
					// This code is executed whenever CRD is created
					w.stateManager.SetAvailable(true)
					w.logger.Info("CRD installed - support enabled", "crd", w.crdName)
				}
			} else {
				if w.stateManager.IsAvailable() {
					// This code is executed whenever CRD is removed
					w.stateManager.SetAvailable(false)
					w.logger.Info("CRD removed - support disabled", "crd", w.crdName)
				}
			}
		}
	}
}
