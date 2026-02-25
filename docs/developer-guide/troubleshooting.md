# Troubleshooting


## Deployment Not Scaling Up

**Symptom**: Deployment remains at 0 replicas despite pending requests.

**Possible Causes**:

1. **InferencePool datastore is empty**:
   ```bash
   # Check if InferencePool exists and is reconciled
   kubectl get inferencepool
   ```
   
   **Solution**: Ensure InferencePool is created and reconciled before creating VariantAutoscaling.

2. **Labels mismatch**:
   ```bash
   # Check deployment labels
   kubectl get deployment llama-8b-deployment -o jsonpath='{.spec.template.metadata.labels}'
   
   # Check InferencePool selector
   kubectl get inferencepool llama-pool -o jsonpath='{.spec.selector}'
   ```
   
   **Solution**: Ensure deployment labels match InferencePool selector.

3. **EPP metrics source not available**:
   ```bash
   # Check if EPP service exists
   kubectl get svc | grep epp
   ```
   
   **Solution**: Verify EndpointPicker service is running and metrics are being collected.

4. **No pending requests in queue**:

   Extract the Bearer token from the EPP metrics reader secret:
   ```bash
   TOKEN=$(kubectl -n workload-variant-autoscaler-system get secret workload-variant-autoscaler-epp-metrics-token -o jsonpath='{.data.token}' | base64 --decode)
   ```

   Port-forward the EPP metrics service to localhost:9090:

   ```bash
   kubectl port-forward svc/epp 9090:9090
   ```

   In a separate terminal, query the metrics endpoint:
   ```bash
   curl -H "Authorization: Bearer $TOKEN" localhost:9090/metrics | grep inference_extension_flow_control_queue_size
   ```

   **Solution**: Verify requests are being sent to the correct model endpoint.

## Slow Scale-Up Response

**Symptom**: Deployment takes too long to scale up from zero.

**Possible Causes**:

1. **High concurrent processing load**:
   
   This can happen when there are many variants that are scaled down to zero, causing the scale-from-zero engine to process multiple scaling decisions simultaneously.
   
   **Solution**: Increase `SCALE_FROM_ZERO_ENGINE_MAX_CONCURRENCY`:

   Add the environment variable to the WVA controller deployment:

   ```yaml
   apiVersion: apps/v1
   kind: Deployment
   metadata:
      name: workload-variant-autoscaler-controller
      namespace: workload-variant-autoscaler-system
   spec:
      template:
         spec:
            containers:
            - name: manager
            env:
            - name: SCALE_FROM_ZERO_ENGINE_MAX_CONCURRENCY
              value: "50"  # Increase for larger clusters
   ```

   Or via Helm:

   ```bash
   helm upgrade -i workload-variant-autoscaler ./charts/workload-variant-autoscaler \
   --namespace workload-variant-autoscaler-system \
   --set controller.env.SCALE_FROM_ZERO_ENGINE_MAX_CONCURRENCY=50
   ```

2. **Inference gateway not receiving requests**:
   
   **Solution**: Verify that requests are being routed through the inference gateway and not directly to model server endpoints.