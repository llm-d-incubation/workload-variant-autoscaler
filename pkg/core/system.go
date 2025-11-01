package core

import (
	"bytes"
	"fmt"

	"github.com/llm-d-incubation/workload-variant-autoscaler/pkg/config"
)

var (
	// a static reference to the singleton system object
	TheSystem *System
)

func GetAccelerator(name string) *Accelerator {
	return TheSystem.Accelerator(name)
}

func GetModel(name string) *Model {
	return TheSystem.Model(name)
}

func GetServiceClass(name string) *ServiceClass {
	return TheSystem.ServiceClass(name)
}

func GetServer(name string) *Server {
	return TheSystem.Server(name)
}

func GetAccelerators() map[string]*Accelerator {
	return TheSystem.accelerators
}

func GetModels() map[string]*Model {
	return TheSystem.models
}

func GetServers() map[string]*Server {
	return TheSystem.servers
}

func GetCapacities() map[string]int32 {
	return TheSystem.capacity
}

// System comprising all accelerators, models, service classes, and servers
type System struct {
	accelerators   map[string]*Accelerator
	models         map[string]*Model
	serviceClasses map[string]*ServiceClass
	servers        map[string]*Server

	capacity           map[string]int32             // available count of accelerator types
	allocationByType   map[string]*AllocationByType // number of allocated accelerator types
	allocationSolution *config.AllocationSolution
}

// Allocation data about an accelerator type
type AllocationByType struct {
	name  string  // name of accelerator type
	count int32   // total number of this type
	limit int32   // maximum number of this type
	cost  float32 // total cost of this type
}

// Create a new system
func NewSystem() *System {
	return &System{
		accelerators:   make(map[string]*Accelerator),
		models:         make(map[string]*Model),
		serviceClasses: make(map[string]*ServiceClass),
		servers:        make(map[string]*Server),

		capacity:           make(map[string]int32),
		allocationByType:   make(map[string]*AllocationByType),
		allocationSolution: nil,
	}
}

// Set system from spec
func (s *System) SetFromSpec(d *config.SystemSpec) *config.OptimizerSpec {
	s.SetAcceleratorsFromSpec(&d.Accelerators)
	s.SetModelsFromSpec(&d.Models)
	s.SetServiceClassesFromSpec(&d.ServiceClasses)
	s.SetServersFromSpec(&d.Servers)
	s.SetCapacityFromSpec(&d.Capacity)
	return &d.Optimizer.Spec
}

// Set accelerators from spec
func (s *System) SetAcceleratorsFromSpec(d *config.AcceleratorData) {
	for _, v := range d.Spec {
		s.AddAcceleratorFromSpec(v)
	}
}

// Add an accelerator (replace if already exists)
func (s *System) AddAcceleratorFromSpec(spec config.AcceleratorSpec) {
	s.accelerators[spec.Name] = NewAcceleratorFromSpec(&spec)
}

// Remove an accelerator
func (s *System) RemoveAccelerator(name string) error {
	if s.accelerators[name] == nil {
		return fmt.Errorf("accelerator %s not found", name)
	}
	delete(s.accelerators, name)
	return nil
}

// Set capacity count from spec
func (s *System) SetCapacityFromSpec(d *config.CapacityData) {
	for _, v := range d.Count {
		s.SetCountFromSpec(v)
	}
}

// Set capacity count for an accelerator type
func (s *System) SetCountFromSpec(spec config.AcceleratorCount) {
	s.capacity[spec.Type] = int32(spec.Count)
}

// Set models from spec
func (s *System) SetModelsFromSpec(d *config.ModelData) {
	for _, pd := range d.PerfData {
		modelName := pd.Name
		var model *Model
		if model = s.models[modelName]; model == nil {
			model = s.AddModel(modelName)
		}
		model.AddPerfDataFromSpec(&pd)
	}
}

// Add a model (replace if already exists)
func (s *System) AddModel(name string) *Model {
	model := NewModel(name)
	s.models[name] = model
	return model
}

// Remove a model
func (s *System) RemoveModel(name string) error {
	if s.models[name] == nil {
		return fmt.Errorf("model %s not found", name)
	}
	delete(s.models, name)
	return nil
}

// Set servers from spec
func (s *System) SetServersFromSpec(d *config.ServerData) {
	for _, v := range d.Spec {
		s.servers[v.Name] = NewServerFromSpec(&v)
	}
}

// Add a server (replace if already exists)
func (s *System) AddServerFromSpec(spec config.ServerSpec) {
	s.servers[spec.Name] = NewServerFromSpec(&spec)
}

// Remove a server
func (s *System) RemoveServer(name string) error {
	if s.servers[name] == nil {
		return fmt.Errorf("server %s not found", name)
	}
	delete(s.servers, name)
	return nil
}

// Set service classes from spec
func (s *System) SetServiceClassesFromSpec(d *config.ServiceClassData) {
	for _, scSpec := range d.Spec {
		s.serviceClasses[scSpec.Name] = NewServiceClassFromSpec(&scSpec)
	}
}

// Add a service class (replace if already exists)
func (s *System) AddServiceClass(name string, priority int) {
	s.serviceClasses[name] = NewServiceClass(name, priority)
}

// Remove a service class
func (s *System) RemoveServiceClass(name string) error {
	if s.serviceClasses[name] == nil {
		return fmt.Errorf("service class %s not found", name)
	}
	delete(s.serviceClasses, name)
	return nil
}

// Get all accelerators
func (s *System) Accelerators() map[string]*Accelerator {
	return s.accelerators
}

// Get all models
func (s *System) Models() map[string]*Model {
	return s.models
}

// Get all service classes
func (s *System) ServiceClasses() map[string]*ServiceClass {
	return s.serviceClasses
}

// Get all servers
func (s *System) Servers() map[string]*Server {
	return s.servers
}

// Get accelerator object for a given accelerator name; nil if doesn't exist
func (s *System) Accelerator(name string) *Accelerator {
	return s.accelerators[name]
}

// Get model object for a given model name; nil if doesn't exist
func (s *System) Model(name string) *Model {
	return s.models[name]
}

// Get service class object for a given service class name; nil if doesn't exist
func (s *System) ServiceClass(name string) *ServiceClass {
	return s.serviceClasses[name]
}

// Get server object for a given server name; nil if doesn't exist
func (s *System) Server(name string) *Server {
	return s.servers[name]
}

// Get capacities of accelerator types
func (s *System) Capacities() map[string]int32 {
	return s.capacity
}

// Get capacity of an accelerator type
func (s *System) Capacity(name string) (int32, bool) {
	if cap, exists := s.capacity[name]; !exists {
		return 0, false
	} else {
		return cap, true
	}
}

// Remove capacity of an accelerator type
func (s *System) RemoveCapacity(name string) bool {
	if _, exists := s.capacity[name]; !exists {
		return false
	}
	delete(s.capacity, name)
	return true
}

// Calculate basic parameters
func (s *System) Calculate() {
	for _, g := range s.accelerators {
		g.Calculate()
	}
	for _, m := range s.models {
		m.Calculate(s.accelerators)
	}
	for _, v := range s.servers {
		v.Calculate(s.accelerators)
	}
}

// Accumulate allocation data by accelerator type
func (s *System) AllocateByType() {
	s.allocationByType = map[string]*AllocationByType{}
	for _, server := range s.Servers() {
		modelName := server.ModelName()
		serverAlloc := server.Allocation()
		if serverAlloc == nil {
			continue
		}
		accName := serverAlloc.accelerator
		acc := s.accelerators[accName]
		model := s.Model(modelName)
		if acc == nil || model == nil {
			continue
		}
		nameType := acc.Type()
		var alloc *AllocationByType
		var exists bool
		if alloc, exists = s.allocationByType[nameType]; !exists {
			alloc = &AllocationByType{
				name:  nameType,
				count: 0,
				limit: s.capacity[nameType],
				cost:  0,
			}
		}
		alloc.count += serverAlloc.numReplicas * model.numInstances[accName] * acc.Multiplicity()
		alloc.cost += serverAlloc.cost
		s.allocationByType[nameType] = alloc
	}
}

// generate json allocation solution for all servers in the system
func (s *System) GenerateSolution() *config.AllocationSolution {
	allocationSolution := config.AllocationSolution{
		Spec: make(map[string]config.AllocationData),
	}
	for serverName, server := range s.servers {
		serverAlloc := server.Allocation()
		if serverAlloc == nil {
			continue
		}
		load := server.Load()
		allocData := serverAlloc.AllocationData()
		allocData.Load = *load
		allocationSolution.Spec[serverName] = *allocData
	}
	s.allocationSolution = &allocationSolution
	return &allocationSolution
}

func (a *AllocationByType) String() string {
	var b bytes.Buffer
	fmt.Fprintf(&b, "name=%s, count=%d, limit=%d, cost=%v", a.name, a.count, a.limit, a.cost)
	return b.String()
}

func (s *System) String() string {
	var b bytes.Buffer
	// b.WriteString("Accelerators: \n")
	// for _, g := range s.accelerators {
	// 	fmt.Fprintln(&b, g)
	// }
	// fmt.Fprintf(&b, "capacity=%v \n", s.capacity)
	// b.WriteString("Models: \n")
	// for _, m := range s.models {
	// 	fmt.Fprintln(&b, m)
	// }
	// b.WriteString("ServiceClasses: \n")
	// for _, c := range s.serviceClasses {
	// 	fmt.Fprintln(&b, c)
	// }
	// b.WriteString("Servers: \n")
	// for _, s := range s.servers {
	// 	fmt.Fprintln(&b, s)
	// }

	b.WriteString("Solution: \n")
	totalCost := float32(0)
	for serverName, server := range s.Servers() {
		srvClassName := server.ServiceClassName()
		modelName := server.ModelName()
		load := server.Load()
		svc := s.serviceClasses[srvClassName]
		if load == nil || svc == nil {
			continue
		}
		target := svc.ModelTarget(modelName)
		if target == nil {
			continue
		}
		alloc := server.Allocation()
		if alloc == nil {
			fmt.Fprintf(&b, "s=%s; c=%s; m=%s; no feasible allocation! \n", serverName, srvClassName, modelName)
			continue
		}
		totalCost += alloc.cost
		rate := load.ArrivalRate
		inTokens := load.AvgInTokens
		outTokens := load.AvgOutTokens
		fmt.Fprintf(&b, "s=%s; c=%s; m=%s; rate=%v; inTk=%d; outTk=%d; sol=%d, sat=%v, alloc=%v; ",
			serverName, srvClassName, modelName, rate, inTokens, outTokens, len(server.allAllocations), server.Saturated(), alloc)
		fmt.Fprintf(&b, "slo-itl=%v, slo-ttft=%v, slo-tps=%v \n", target.ITL, target.TTFT, target.TPS)
	}

	b.WriteString("AllocationByType: \n")
	for _, a := range s.allocationByType {
		fmt.Fprintf(&b, "%v \n", a)
	}
	fmt.Fprintf(&b, "totalCost=%v \n", totalCost)

	return b.String()
}
