package core

import (
	"bytes"
	"fmt"
	"math"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/pkg/analyzer"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/pkg/config"
)

// Allocation details of an accelerator to a server
type Allocation struct {
	accelerator string  // name of accelerator
	numReplicas int     // number of server replicas
	batchSize   int     // max batch size
	cost        float32 // cost of this allocation
	value       float32 // value of this allocation
	itl         float32 // expected average token decode time (msec)
	ttft        float32 // expected average request queueing and prefill times (msec)
	rho         float32 // average concurrently running requests / max batch size

	maxArrvRatePerReplica float32 // maximum arrival rate per replica (req/msec)
}

// Create an allocation of an accelerator to a server; nil if not feasible
func CreateAllocation(serverName string, gName string) *Allocation {
	var (
		acc *Accelerator

		server *Server
		load   *config.ServerLoadSpec

		model *Model
		perf  *config.ModelAcceleratorPerfData

		svc    *ServiceClass
		target *Target
	)

	// get accelerator info
	if acc = GetAccelerator(gName); acc == nil {
		return nil
	}

	// get server info
	if server = GetServer(serverName); server == nil {
		return nil
	}
	if load = server.Load(); load == nil || load.ArrivalRate < 0 ||
		load.AvgInTokens < 0 || load.AvgOutTokens < 0 {
		return nil
	}

	// get model info
	modelName := server.ModelName()
	if model = GetModel(modelName); model == nil {
		return nil
	}
	if perf = model.PerfData(gName); perf == nil {
		return nil
	}

	// get service class info
	if svc = GetServiceClass(server.ServiceClassName()); svc == nil {
		return nil
	}
	if target = svc.ModelTarget(modelName); target == nil {
		return nil
	}

	// handle zero traffic case
	if load.ArrivalRate == 0 || load.AvgOutTokens == 0 {
		return zeroLoadAllocation(server, model, acc, perf)
	}

	// calculate max batch size (N) based on average request length (K)
	K := load.AvgOutTokens

	// use maxBatchSize from configured value or scaled performance data
	var N int
	if server.maxBatchSize > 0 {
		N = server.maxBatchSize
	} else {
		N = max(perf.MaxBatchSize*perf.AtTokens/K, 1)
	}
	maxQueue := N * config.MaxQueueToBatchRatio

	// create queue analyzer
	qConfig := &analyzer.Configuration{
		MaxBatchSize: N,
		MaxQueueSize: maxQueue,
		ServiceParms: &analyzer.ServiceParms{
			Alpha: perf.ServiceParms.Alpha,
			Beta:  perf.ServiceParms.Beta,
			Gamma: perf.ServiceParms.Gamma,
		},
	}

	requestData := &analyzer.RequestSize{
		AvgInputTokens:  float32(load.AvgInTokens),
		AvgOutputTokens: float32(K),
	}

	queueAnalyzer, err := analyzer.NewQueueAnalyzer(qConfig, requestData)
	if err != nil {
		fmt.Println(err)
		return nil
	}

	targetPerf := &analyzer.TargetPerf{
		TargetTTFT: target.TTFT,
		TargetITL:  target.ITL,
		TargetTPS:  target.TPS,
	}

	// determine max rates to satisfy targets
	_, metrics, _, err := queueAnalyzer.Size(targetPerf)
	if err != nil {
		// fmt.Println(err)
		return nil
	}
	rateStar := metrics.Throughput

	// calculate number of replicas
	var totalRate float32
	if target.TPS == 0 {
		totalRate = load.ArrivalRate / 60
	} else {
		totalRate = target.TPS / float32(K)
	}
	numReplicas := int(math.Ceil(float64(totalRate) / float64(rateStar)))
	numReplicas = max(numReplicas, server.minNumReplicas)

	// calculate cost
	totalNumInstances := model.NumInstances(gName) * numReplicas
	cost := acc.Cost() * float32(totalNumInstances)

	// analyze queue of one replica
	rate := totalRate / float32(numReplicas)
	metrics, err = queueAnalyzer.Analyze(rate)
	if err != nil {
		fmt.Println(err)
		return nil
	}
	rho := metrics.Rho
	itl := metrics.AvgTokenTime
	ttft := metrics.AvgWaitTime + metrics.AvgPrefillTime
	// fmt.Printf("numReplicas=%d; batchSize=%d; rate=%v, itl=%v; ttft=%v; \n", numReplicas, N, rate, itl, ttft)

	alloc := &Allocation{accelerator: gName, numReplicas: numReplicas, batchSize: N,
		cost: cost, itl: itl, ttft: ttft, rho: rho, maxArrvRatePerReplica: rateStar / 1000}
	alloc.SetValue(alloc.cost)
	return alloc
}

func (a *Allocation) Scale(serverName string) (alloc *Allocation, inc int) {
	var (
		acc    *Accelerator
		server *Server
		load   *config.ServerLoadSpec
	)

	// get server info
	if server = GetServer(serverName); server == nil {
		return nil, 0
	}
	if load = server.Load(); load == nil {
		return nil, 0
	}

	// get accelerator info
	gName := a.accelerator
	if acc = GetAccelerator(gName); acc == nil {
		return nil, 0
	}

	// create new allocation
	alloc = CreateAllocation(serverName, gName)
	inc = alloc.numReplicas - a.numReplicas
	return alloc, inc
}

func (a *Allocation) ReAllocate(serverName string) (*Allocation, string) {
	minVal := float32(0)
	var minAlloc *Allocation
	for gName := range GetAccelerators() {
		if alloc := CreateAllocation(serverName, gName); alloc != nil {
			if minVal == 0 || alloc.value < minVal {
				minVal = alloc.value
				minAlloc = alloc
			}
		}
	}
	if minAlloc == nil {
		return nil, ""
	}
	return minAlloc, minAlloc.accelerator
}

func (a *Allocation) Accelerator() string {
	return a.accelerator
}

func (a *Allocation) NumReplicas() int {
	return a.numReplicas
}

func (a *Allocation) SetNumReplicas(n int) {
	a.numReplicas = n
}

func (a *Allocation) MaxBatchSize() int {
	return a.batchSize
}

func (a *Allocation) SetMaxBatchSize(batchSize int) {
	a.batchSize = batchSize
}

func (a *Allocation) MaxArrvRatePerReplica() float32 {
	return a.maxArrvRatePerReplica
}

func (a *Allocation) MaxRPM() float32 {
	return a.maxArrvRatePerReplica * 1000 * 60
}

func (a *Allocation) Cost() float32 {
	return a.cost
}

func (a *Allocation) SetCost(cost float32) {
	a.cost = cost
}

func (a *Allocation) Value() float32 {
	return a.value
}

// Set the value for this allocation (may depend on cost, performance, ...)
func (a *Allocation) SetValue(value float32) {
	a.value = value
}

func (a *Allocation) Saturated(totalRate float32) bool {
	return totalRate > float32(a.numReplicas)*a.MaxRPM()
}

// Allocation in case of zero load
func zeroLoadAllocation(server *Server, model *Model, acc *Accelerator, perf *config.ModelAcceleratorPerfData) *Allocation {

	numReplicas := server.minNumReplicas
	gName := acc.Name()
	if numReplicas == 0 {
		alloc := &Allocation{accelerator: "", numReplicas: 0, batchSize: 0,
			cost: 0, itl: 0, ttft: 0, rho: 0, maxArrvRatePerReplica: 0}
		alloc.SetValue(0)
		return alloc
	}

	maxBatchSize := perf.MaxBatchSize
	if server.maxBatchSize > 0 {
		maxBatchSize = server.maxBatchSize
	}
	totalNumInstances := model.NumInstances(gName) * numReplicas
	cost := acc.Cost() * float32(totalNumInstances)

	// TODO: maxArrvRatePerReplica seems to be meaningless
	decodeTime := perf.ServiceParms.Alpha + perf.ServiceParms.Beta
	maxDecodeTime := perf.ServiceParms.Alpha + perf.ServiceParms.Beta*float32(maxBatchSize)
	prefillTime := perf.ServiceParms.Alpha + perf.ServiceParms.Beta
	maxServTime := prefillTime + maxDecodeTime
	maxArrvRatePerReplica := float32(maxBatchSize) / maxServTime

	alloc := &Allocation{accelerator: gName, numReplicas: numReplicas, batchSize: maxBatchSize,
		cost: cost, itl: decodeTime, ttft: prefillTime, rho: 0, maxArrvRatePerReplica: maxArrvRatePerReplica}
	alloc.SetValue(alloc.cost)
	return alloc
}

// Calculate penalty for transitioning from this allocation (a) to another allocation (b)
func (a *Allocation) TransitionPenalty(b *Allocation) float32 {
	if a.accelerator == b.accelerator {
		if a.numReplicas == b.numReplicas {
			return 0
		} else {
			return b.cost - a.cost
		}
	}
	return config.AccelPenaltyFactor*(a.cost+b.cost) + (b.cost - a.cost)
}

func (a *Allocation) Clone() *Allocation {
	return &Allocation{
		accelerator: a.accelerator,
		numReplicas: a.numReplicas,
		batchSize:   a.batchSize,
		cost:        a.cost,
		value:       a.value,
		itl:         a.itl,
		ttft:        a.ttft,
		rho:         a.rho,

		maxArrvRatePerReplica: a.maxArrvRatePerReplica,
	}
}

func (a *Allocation) AllocationData() *config.AllocationData {
	return &config.AllocationData{
		Accelerator: a.accelerator,
		NumReplicas: a.numReplicas,
		MaxBatch:    a.batchSize,
		Cost:        a.cost,
		ITLAverage:  a.itl,
		TTFTAverage: a.ttft,
	}
}

func AllocationFromData(data *config.AllocationData) *Allocation {
	return &Allocation{
		accelerator: data.Accelerator,
		numReplicas: data.NumReplicas,
		batchSize:   data.MaxBatch,
		cost:        data.Cost,
		itl:         data.ITLAverage,
		ttft:        data.TTFTAverage,
	}
}

func (a *Allocation) String() string {
	return fmt.Sprintf("{acc=%s; numRep=%d; maxBatch=%d; cost=%v, val=%v, itl=%v, ttft=%v, rho=%v, maxRPM=%v}",
		a.accelerator, a.numReplicas, a.batchSize, a.cost, a.value, a.itl, a.ttft, a.rho, a.MaxRPM())
}

// Orchestration difference between two allocations
type AllocationDiff struct {
	oldAccelerator string
	newAccelerator string
	oldNumReplicas int
	newNumReplicas int
	costDiff       float32
}

func CreateAllocationDiff(a *Allocation, b *Allocation) *AllocationDiff {
	if a == nil && b == nil {
		return nil
	}
	oldAccelerator := "none"
	newAccelerator := "none"
	oldNumReplicas := 0
	newNumReplicas := 0
	oldCost := float32(0)
	newCost := float32(0)
	if a != nil {
		oldAccelerator = a.accelerator
		oldNumReplicas = a.numReplicas
		oldCost = a.cost
	}
	if b != nil {
		newAccelerator = b.accelerator
		newNumReplicas = b.numReplicas
		newCost = b.cost
	}
	return &AllocationDiff{
		oldAccelerator: oldAccelerator,
		newAccelerator: newAccelerator,
		oldNumReplicas: oldNumReplicas,
		newNumReplicas: newNumReplicas,
		costDiff:       newCost - oldCost,
	}
}

func (d *AllocationDiff) String() string {
	var b bytes.Buffer
	fmt.Fprintf(&b, "{ %s -> %s, %d -> %d, %v }",
		d.oldAccelerator, d.newAccelerator, d.oldNumReplicas, d.newNumReplicas, d.costDiff)
	return b.String()
}
