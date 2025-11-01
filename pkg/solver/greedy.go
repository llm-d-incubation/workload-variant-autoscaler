package solver

import (
	"bytes"
	"cmp"
	"fmt"
	"maps"
	"math"
	"slices"

	"github.com/llm-d-incubation/workload-variant-autoscaler/pkg/config"
	"github.com/llm-d-incubation/workload-variant-autoscaler/pkg/core"
)

// Entry for a server, used during greedy allocation
type serverEntry struct {
	serverName  string             // server name
	priority    int                // priority of service class for server
	curIndex    int                // current index in allocation list
	allocations []*core.Allocation // ordered list of allocations
	delta       float32            // delta penalty if current allocation not allowed and next allocation is allowed
}

func (e *serverEntry) String() string {
	var b bytes.Buffer
	fmt.Fprintf(&b, "sName=%s, prio=%d, curIndex=%d, delta=%v, allocations=%v \n",
		e.serverName, e.priority, e.curIndex, e.delta, e.allocations)
	return b.String()
}

// sorting function for server entries
type ServerEntriesOrder func(a, b *serverEntry) int

// Find optimal allocations using greedy algorithm, assuming limited accelerator capacity
func (s *Solver) SolveGreedy() {

	// make a copy of count of available accelerator types
	available := make(map[string]int32)
	maps.Copy(available, core.GetCapacities())

	// create entries for all servers, sorting candidate allocations per server
	entries := make([]*serverEntry, 0)
	for serverName, server := range core.GetServers() {
		server.RemoveAllocation()
		allAllocs := server.AllAllocations()
		if len(allAllocs) == 0 {
			continue
		}
		e := &serverEntry{
			serverName:  serverName,
			priority:    server.Priority(),
			curIndex:    0,
			allocations: make([]*core.Allocation, len(allAllocs)),
			delta:       0,
		}
		i := 0
		for _, alloc := range allAllocs {
			e.allocations[i] = alloc
			i++
		}
		slices.SortFunc(e.allocations, func(a, b *core.Allocation) int {
			return cmp.Compare(a.Value(), b.Value())
		})
		if len(e.allocations) > 1 {
			// value is difference between this and next allocation
			e.delta = e.allocations[1].Value() - e.allocations[0].Value()
		} else {
			// last choice, large value for not selecting this allocation
			e.delta = math.MaxFloat32
		}
		entries = append(entries, e)
	}

	// sorting function for server entries
	// - straight priorities, then delta values
	orderFunc := func(a, b *serverEntry) int {
		if a.priority == b.priority {
			if a.delta == b.delta {
				return cmp.Compare(b.allocations[b.curIndex].Value(), a.allocations[a.curIndex].Value())
			}
			return cmp.Compare(b.delta, a.delta)
		} else {
			return cmp.Compare(a.priority, b.priority)
		}
	}
	// sort server entries
	slices.SortFunc(entries, orderFunc)

	// allocate
	if s.optimizerSpec.DelayedBestEffort {
		// allocate to all servers
		unallocated := allocate(entries, available, orderFunc)
		// best effort allocation to all remaining servers
		bestEffort(unallocated, available, s.optimizerSpec.SaturationPolicy)
	} else {
		groupEntries := makePriorityGroups(entries)
		for _, group := range groupEntries {
			// allocate to servers in priority group
			unallocated := allocate(group, available, orderFunc)
			// best effort allocation to servers in priority group
			bestEffort(unallocated, available, s.optimizerSpec.SaturationPolicy)
		}
	}
}

// allocate, satisfying SLO requirements, returning servers that did not receive any allocation
func allocate(entries []*serverEntry,
	available map[string]int32,
	orderFunc ServerEntriesOrder) (unallocatedEntries []*serverEntry) {

	unallocatedEntries = make([]*serverEntry, 0)
	// start allocation greedily, in order
	for len(entries) > 0 {
		// pick top entry and remove from list
		top := entries[0]
		entries = entries[1:]
		// check if no more candidate allocations
		if len(top.allocations) == 0 {
			continue
		}

		// check if current allocation in entry can be satisfied
		serverName := top.serverName
		server := core.GetServer(serverName)
		if server == nil {
			continue
		}
		model := core.GetModel(server.ModelName())
		if model == nil {
			continue
		}
		alloc := top.allocations[top.curIndex]
		gName := alloc.Accelerator()
		acc := core.GetAccelerator(gName)
		if acc == nil {
			continue
		}
		tName := acc.Type()
		unitsPerReplica := model.NumInstances(gName) * acc.Multiplicity()
		count := alloc.NumReplicas() * unitsPerReplica

		// check if accelerator type of current allocation is available, allocate
		if available[tName] >= count {
			available[tName] -= count
			server.SetAllocation(alloc)
		} else {
			// otherwise, move to next candidate allocation
			top.curIndex++
			if top.curIndex+1 < len(top.allocations) {
				// not last allocation, calculate delta
				top.delta = top.allocations[top.curIndex+1].Value() - top.allocations[top.curIndex].Value()
			} else if top.curIndex == len(top.allocations) {
				// no more allocations, could not satisfy any, add server to unallocated list
				unallocatedEntries = append(unallocatedEntries, top)
				continue
			} else {
				// last allocation, set large delta value
				top.delta = math.MaxFloat32
			}
			// reorder server entries
			i, _ := slices.BinarySearchFunc(entries, top, orderFunc)
			entries = slices.Insert(entries, i, top)
		}
	}
	return unallocatedEntries
}

// give best effort allocation to unallocated servers according to saturation policy
func bestEffort(unallocatedServers []*serverEntry, available map[string]int32, policy string) {
	switch config.SaturatedAllocationPolicyEnum(policy) {

	// allocate exhaustively to servers in priority ordering
	case config.PriorityExhaustive:
		allocateMaximally(unallocatedServers, available)

	// allocate in round-robin fashion within priority groups
	case config.PriorityRoundRobin:
		priorityGroups := makePriorityGroups(unallocatedServers)
		for _, group := range priorityGroups {
			allocateEqually(group, available)
		}

	// allocate in round-robin fashion across all servers
	case config.RoundRobin:
		allocateEqually(unallocatedServers, available)

	// do not allocate beyond satisfying SLOs
	case config.None:
	}
}

// Allocate remaining accelerators among unallocated servers
//   - priority ordering: one server at a time exhaustively, until no resources to satisfy requirements
func allocateMaximally(serverEntries []*serverEntry, available map[string]int32) {
	// fmt.Println("Unallocated server entries: ", serverEntries)
	for _, entry := range serverEntries {
		for _, alloc := range entry.allocations {
			accName := alloc.Accelerator()
			serverName := entry.serverName
			server := core.GetServer(serverName)
			model := core.GetModel(server.ModelName())
			if acc := core.GetAccelerator(accName); acc != nil && model != nil && server != nil {
				if unitsPerReplica := model.NumInstances(accName) * acc.Multiplicity(); unitsPerReplica > 0 {
					maxReplicas := available[acc.Type()] / unitsPerReplica
					if maxReplicas = min(maxReplicas, alloc.NumReplicas()); maxReplicas > 0 {
						curNumReplicas := alloc.NumReplicas()
						// adjust cost and value
						factor := float32(maxReplicas) / float32(curNumReplicas)
						alloc.SetCost(alloc.Cost() * factor)
						alloc.SetValue(alloc.Value() * factor)
						alloc.SetNumReplicas(maxReplicas)
						server.SetAllocation(alloc)
						count := maxReplicas * unitsPerReplica
						available[acc.Type()] -= count
						// fmt.Printf("updated allocation: server=%s, acc=%s, maxReplicas=%d, type=%s, count=%d \n",
						// 	serverName, accName, maxReplicas, acc.Type(), count)
						break
					}
				}
			}
		}
	}
}

type serverAllocationTicket struct {
	entry  *serverEntry
	active bool // receiving allocation in round-robin
	server *core.Server
	model  *core.Model

	accType         string // type of accelerator allocated to server
	unitsPerReplica int32
	numReplicas     int32
	finalAlloc      *core.Allocation
}

// Allocate remaining accelerators among a group of unallocated servers
//   - round-robin allocation to members in group until no resources to satisfy requirements
func allocateEqually(serverEntries []*serverEntry, available map[string]int32) {
	// fmt.Println("Unallocated server entries: ", serverEntries)

	// create allocation tickets for all valid members in group
	tickets := make(map[string]*serverAllocationTicket)
	for _, serverEntry := range serverEntries {
		serverName := serverEntry.serverName
		server := core.GetServer(serverName)
		model := core.GetModel(server.ModelName())
		if model == nil || server == nil {
			continue
		}
		tickets[serverEntry.serverName] = &serverAllocationTicket{
			entry:  serverEntry,
			active: false,
			server: server,
			model:  model,
		}
	}

	// visit members in round-robin way
	allocatedTickets := make(map[string]*serverAllocationTicket)
	for len(tickets) > 0 {
		for _, serverEntry := range serverEntries {
			serverName := serverEntry.serverName
			var ticket *serverAllocationTicket
			if ticket = tickets[serverName]; ticket == nil {
				continue
			}
			// determine candidate allocation for not yet processed members
			if !ticket.active {
				for _, alloc := range serverEntry.allocations {
					accName := alloc.Accelerator()
					if acc := core.GetAccelerator(accName); acc != nil {
						unitsPerReplica := ticket.model.NumInstances(accName) * acc.Multiplicity()
						if unitsPerReplica > 0 && available[acc.Type()] >= unitsPerReplica {
							ticket.active = true
							ticket.accType = acc.Type()
							ticket.unitsPerReplica = unitsPerReplica
							ticket.finalAlloc = alloc
							break
						}
					}
				}
				// check if no candidate allocation was found
				if !ticket.active {
					delete(tickets, serverName)
					continue
				}
			}
			// make one allocation (replica) to member
			replicasAvailable := available[ticket.accType] / ticket.unitsPerReplica
			if replicasAllocatable := min(replicasAvailable, ticket.finalAlloc.NumReplicas()); replicasAllocatable > 0 {
				ticket.numReplicas++
				available[ticket.accType] -= ticket.unitsPerReplica
				allocatedTickets[serverName] = ticket
			} else {
				// remove ticket if can no longer allocate
				delete(tickets, serverName)
			}
		}
	}
	// update allocated members
	for _, ticket := range allocatedTickets {
		alloc := ticket.finalAlloc
		numReplicas := ticket.numReplicas
		curNumReplicas := alloc.NumReplicas()
		// adjust cost and value
		factor := float32(numReplicas) / float32(curNumReplicas)
		alloc.SetCost(alloc.Cost() * factor)
		alloc.SetValue(alloc.Value() * factor)
		alloc.SetNumReplicas(numReplicas)
		ticket.server.SetAllocation(alloc)
		// count := ticket.numReplicas * ticket.unitsPerReplica
		// fmt.Printf("updated allocation: server=%s, acc=%s, accCount=%d, type=%s, count=%d \n",
		// 	ticket.server.Name(), alloc.Accelerator(), ticket.numReplicas, ticket.accType, count)
	}
}

// Partition a list of server entries into groups of same priority
//   - each group has same server priority
//   - groups are ordered by priority
func makePriorityGroups(serverEntries []*serverEntry) [][]*serverEntry {
	serverEntryGroups := make([][]*serverEntry, 0)
	numServerEntries := len(serverEntries)

	// make groups of same priority servers
	index := 0
	for index < numServerEntries {
		// start new group
		group := make([]*serverEntry, 0)
		group = append(group, serverEntries[index])
		groupPriority := serverEntries[index].priority
		index++
		for index < numServerEntries && serverEntries[index].priority == groupPriority {
			group = append(group, serverEntries[index])
			index++
		}
		// group completed
		serverEntryGroups = append(serverEntryGroups, group)
	}
	return serverEntryGroups
}
