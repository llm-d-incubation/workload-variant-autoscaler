/*
Copyright 2025 The llm-d Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package scalefromzero

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsV1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta/testrestmapper"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"

	vav1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/api/v1alpha1"
	poolreconciler "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/controller"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/datastore"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils"
	unittestutil "github.com/llm-d/llm-d-workload-variant-autoscaler/test/utils"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	v1 "sigs.k8s.io/gateway-api-inference-extension/api/v1"
	"sigs.k8s.io/gateway-api-inference-extension/apix/v1alpha2"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/common"
	utiltest "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/util/testing"
)

var (
	selector_v1     = map[string]string{"app": "vllm_v1"}
	namespace       = "pool1-ns"
	resourceName    = "resource-name"
	deploymentName  = "deployment-name"
	acceleratorName = "A100"
	modelId         = "unsloth/Meta-Llama-3.1-8B"
	variantCost     = float64(5)
)

func TestSingleInactiveVariant(t *testing.T) {
	gvk := schema.GroupVersionKind{
		Group:   v1.GroupVersion.Group,
		Version: v1.GroupVersion.Version,
		Kind:    "InferencePool",
	}
	pool1 := utiltest.MakeInferencePool("pool1").
		Namespace(namespace).
		Selector(selector_v1).
		TargetPorts(8080).
		EndpointPickerRef("epp-pool1-svc").ObjRef()
	pool1.SetGroupVersionKind(gvk)

	tests := []struct {
		name             string
		pool             *v1.InferencePool
		resourceReplicas int32
		labels           map[string]string
		datastoreSize    int
		wantErr          bool
	}{
		{
			name:             "one inactiveVariant: successful scalefromzero optimization",
			pool:             pool1,
			labels:           map[string]string{"app": "vllm_v1"},
			datastoreSize:    1,
			resourceReplicas: 0,
		},
		{
			name:             "zero inactiveVariant: skipped scalefromzero optimization",
			pool:             pool1,
			labels:           map[string]string{"app": "vllm_v1"},
			datastoreSize:    1,
			resourceReplicas: 1,
		},
		{
			name:             "Error from labels of inferencePool and deployment not matched",
			pool:             pool1,
			labels:           map[string]string{"vllm": "test"},
			datastoreSize:    1,
			resourceReplicas: 0,
			wantErr:          true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pool1 := tt.pool

			va := unittestutil.CreateVariantAutoscalingResource(namespace, resourceName, deploymentName, modelId, acceleratorName, variantCost)
			dp := unittestutil.MakeDeployment(deploymentName, namespace, tt.resourceReplicas, tt.labels)
			svc := unittestutil.MakeService("epp-pool1-svc", namespace)

			scheme := runtime.NewScheme()
			_ = clientgoscheme.AddToScheme(scheme)
			_ = v1alpha2.Install(scheme)
			_ = v1.Install(scheme)
			_ = vav1alpha1.AddToScheme(scheme)
			_ = appsV1.AddToScheme(scheme)
			_ = corev1.AddToScheme(scheme)
			fakeClientInitialObjs := []client.Object{pool1, dp, va, svc}
			fakeDynamicClientInitialObject := []runtime.Object{dp}

			fakeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(fakeClientInitialObjs...).
				Build()

			fakeDynamicClient := dynamicfake.NewSimpleDynamicClient(scheme, fakeDynamicClientInitialObject...)

			// Create a request for the existing resource.
			namespacedName := types.NamespacedName{Name: pool1.Name, Namespace: pool1.Namespace}
			gknn := common.GKNN{
				NamespacedName: namespacedName,
				GroupKind: schema.GroupKind{
					Group: pool1.GroupVersionKind().Group,
					Kind:  pool1.GroupVersionKind().Kind,
				},
			}

			req := ctrl.Request{NamespacedName: namespacedName}
			ctx := context.Background()

			ds := datastore.NewDatastore(nil)
			inferencePoolReconciler := &poolreconciler.InferencePoolReconciler{Client: fakeClient, Datastore: ds, PoolGKNN: gknn}

			// (1) Reconcile inferencePool and store generated endpointPool in the datastore
			if _, err := inferencePoolReconciler.Reconcile(ctx, req); err != nil {
				t.Errorf("Unexpected InferencePool reconcile error: %v", err)
			}

			// Check the size of the datastore
			assert.Equal(t, len(ds.PoolList()), tt.datastoreSize, "There should be one EndpointPool in the datastore")

			// (2) Create scalefromzero engine loop
			mapper := testrestmapper.TestOnlyStaticRESTMapper(scheme, schema.GroupVersion{Group: "apps", Version: "v1"})

			engine := &Engine{
				client:         fakeClient,
				executor:       nil,
				Datastore:      ds,
				DynamicClient:  fakeDynamicClient,
				Mapper:         mapper,
				maxConcurrency: 30,
			}

			// Call the optimize function.
			err := engine.optimize(ctx)
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestMultipleInactiveVariants(t *testing.T) {
	gvk := schema.GroupVersionKind{
		Group:   v1.GroupVersion.Group,
		Version: v1.GroupVersion.Version,
		Kind:    "InferencePool",
	}
	pool1 := utiltest.MakeInferencePool("pool1").
		Namespace(namespace).
		Selector(selector_v1).
		TargetPorts(8080).
		EndpointPickerRef("epp-pool1-svc").ObjRef()
	pool1.SetGroupVersionKind(gvk)

	// Create multiple VAs with different models
	va1 := unittestutil.CreateVariantAutoscalingResource(namespace, "resource-1", "resource-1-deployment", "model-1", acceleratorName, variantCost)
	va2 := unittestutil.CreateVariantAutoscalingResource(namespace, "resource-2", "resource-2-deployment", "model-2", acceleratorName, variantCost)
	va3 := unittestutil.CreateVariantAutoscalingResource(namespace, "resource-3", "resource-3-deployment", "model-3", acceleratorName, variantCost)

	dp1 := unittestutil.MakeDeployment("resource-1-deployment", namespace, 0, selector_v1)
	dp2 := unittestutil.MakeDeployment("resource-2-deployment", namespace, 0, selector_v1)
	dp3 := unittestutil.MakeDeployment("resource-3-deployment", namespace, 0, selector_v1)
	svc := unittestutil.MakeService("epp-pool1-svc", namespace)

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1alpha2.Install(scheme)
	_ = v1.Install(scheme)
	_ = vav1alpha1.AddToScheme(scheme)
	_ = appsV1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	fakeClientInitialObjs := []client.Object{pool1, dp1, dp2, dp3, va1, va2, va3, svc}
	fakeDynamicClientInitialObject := []runtime.Object{dp1, dp2, dp3}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(fakeClientInitialObjs...).
		Build()

	fakeDynamicClient := dynamicfake.NewSimpleDynamicClient(scheme, fakeDynamicClientInitialObject...)

	namespacedName := types.NamespacedName{Name: pool1.Name, Namespace: pool1.Namespace}
	gknn := common.GKNN{
		NamespacedName: namespacedName,
		GroupKind: schema.GroupKind{
			Group: pool1.GroupVersionKind().Group,
			Kind:  pool1.GroupVersionKind().Kind,
		},
	}

	req := ctrl.Request{NamespacedName: namespacedName}
	ctx := context.Background()

	ds := datastore.NewDatastore(nil)
	inferencePoolReconciler := &poolreconciler.InferencePoolReconciler{Client: fakeClient, Datastore: ds, PoolGKNN: gknn}

	if _, err := inferencePoolReconciler.Reconcile(ctx, req); err != nil {
		t.Errorf("Unexpected InferencePool reconcile error: %v", err)
	}

	mapper := testrestmapper.TestOnlyStaticRESTMapper(scheme, schema.GroupVersion{Group: "apps", Version: "v1"})

	engine := &Engine{
		client:         fakeClient,
		executor:       nil,
		Datastore:      ds,
		DynamicClient:  fakeDynamicClient,
		Mapper:         mapper,
		maxConcurrency: 30,
	}

	// Get all inactive VAs
	inactiveVAs, deployments, err := utils.InactiveVariantAutoscaling(ctx, fakeClient)
	require.NoError(t, err)
	assert.Equal(t, 3, len(inactiveVAs), "Should have 3 inactive VAs")

	// Verify deployments map is populated correctly
	assert.NotNil(t, deployments, "Deployments map should not be nil")
	assert.Equal(t, 3, len(deployments), "Should have 3 deployments in the map")

	// Verify each deployment is keyed by namespace/deploymentName
	expectedDeployments := []string{
		namespace + "/resource-1-deployment",
		namespace + "/resource-2-deployment",
		namespace + "/resource-3-deployment",
	}
	for _, expectedKey := range expectedDeployments {
		deployment, found := deployments[expectedKey]
		assert.True(t, found, "Deployment with key %s should be in the map", expectedKey)
		assert.NotNil(t, deployment, "Deployment should not be nil for key %s", expectedKey)
		if deployment != nil {
			assert.Equal(t, namespace, deployment.Namespace, "Deployment namespace should match")
			assert.Equal(t, int32(0), *deployment.Spec.Replicas, "Deployment should have 0 replicas (inactive)")
		}
	}

	// Run optimize - it should handle multiple VAs concurrently
	err = engine.optimize(ctx)
	// No error expected when EPP metrics source is not set up (it just skips processing)
	assert.NoError(t, err)
}

func TestEmptyInactiveVariants(t *testing.T) {
	gvk := schema.GroupVersionKind{
		Group:   v1.GroupVersion.Group,
		Version: v1.GroupVersion.Version,
		Kind:    "InferencePool",
	}
	pool1 := utiltest.MakeInferencePool("pool1").
		Namespace(namespace).
		Selector(selector_v1).
		TargetPorts(8080).
		EndpointPickerRef("epp-pool1-svc").ObjRef()
	pool1.SetGroupVersionKind(gvk)

	// Create VA with non-zero replicas (active)
	va := unittestutil.CreateVariantAutoscalingResource(namespace, resourceName, resourceName+"-deployment", modelId, acceleratorName, variantCost)
	dp := unittestutil.MakeDeployment(resourceName+"-deployment", namespace, 1, selector_v1) // 1 replica = active
	svc := unittestutil.MakeService("epp-pool1-svc", namespace)

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1alpha2.Install(scheme)
	_ = v1.Install(scheme)
	_ = vav1alpha1.AddToScheme(scheme)
	_ = appsV1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	fakeClientInitialObjs := []client.Object{pool1, dp, va, svc}
	fakeDynamicClientInitialObject := []runtime.Object{dp}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(fakeClientInitialObjs...).
		Build()

	fakeDynamicClient := dynamicfake.NewSimpleDynamicClient(scheme, fakeDynamicClientInitialObject...)

	namespacedName := types.NamespacedName{Name: pool1.Name, Namespace: pool1.Namespace}
	gknn := common.GKNN{
		NamespacedName: namespacedName,
		GroupKind: schema.GroupKind{
			Group: pool1.GroupVersionKind().Group,
			Kind:  pool1.GroupVersionKind().Kind,
		},
	}

	req := ctrl.Request{NamespacedName: namespacedName}
	ctx := context.Background()

	ds := datastore.NewDatastore(nil)
	inferencePoolReconciler := &poolreconciler.InferencePoolReconciler{Client: fakeClient, Datastore: ds, PoolGKNN: gknn}

	if _, err := inferencePoolReconciler.Reconcile(ctx, req); err != nil {
		t.Errorf("Unexpected InferencePool reconcile error: %v", err)
	}

	mapper := testrestmapper.TestOnlyStaticRESTMapper(scheme, schema.GroupVersion{Group: "apps", Version: "v1"})

	engine := &Engine{
		client:         fakeClient,
		executor:       nil,
		Datastore:      ds,
		DynamicClient:  fakeDynamicClient,
		Mapper:         mapper,
		maxConcurrency: 30,
	}

	// Should complete without error when no inactive VAs exist
	err := engine.optimize(ctx)
	assert.NoError(t, err, "Should not error when no inactive VAs exist")
}
