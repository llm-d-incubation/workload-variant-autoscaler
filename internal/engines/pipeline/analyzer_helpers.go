package pipeline

import (
	"context"
	"math"
	"sort"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/interfaces"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
)

// applyAllocation subtracts the capacity provided by n replicas of variant v
// from each analyzer's Remaining counter. Clamps to 0. The slice is the working
// allocation state; Result.RequiredCapacity is never mutated.
//
// Contract: Remaining/Spare are engine-calibrated on entry (via the universal
// threshold post-step). Helpers do not read or mutate PendingReplicas.
func applyAllocation(s []NamedAnalyzerResult, v string, n int) {
	for i := range s {
		if s[i].Result == nil {
			continue
		}
		prc := prcForVariant(s[i].Result, v)
		if prc <= 0 {
			continue
		}
		s[i].Remaining -= float64(n) * prc
		if s[i].Remaining < 0 {
			s[i].Remaining = 0
		}
	}
}

// saturationEntry returns the saturation analyzer's result from s, or nil if not present.
// The saturation entry is the keeper of per-variant metadata (Cost, AcceleratorName, Role,
// replica counts) that the optimizer uses for variant selection and GPU accounting.
// TODO: remove the sat_v2 special role once all analyzers populate variant metadata.
func saturationEntry(s []NamedAnalyzerResult) *interfaces.AnalyzerResult {
	for _, e := range s {
		if e.Name == interfaces.SaturationAnalyzerName {
			return e.Result
		}
	}
	return nil
}

// prcForVariant returns the PerReplicaCapacity for variant v in result r.
// Returns 0 if the variant is not present.
func prcForVariant(r *interfaces.AnalyzerResult, v string) float64 {
	for _, vc := range r.VariantCapacities {
		if vc.VariantName == v {
			return vc.PerReplicaCapacity
		}
	}
	return 0
}

// =============================================================================
// Paired helpers — disaggregated (P/D) models
// =============================================================================

// initRoleState initialises picker-local role state for one model's allocation pass.
// It unifies disaggregated and non-disaggregated models into one (model, role) view:
//
//   - Disaggregated (RoleCapacities != nil): roles = sorted keys of RoleCapacities;
//     per-role RC → pickerState[i][role]; per-role SC → s[i].RoleSpare[role].
//   - Non-disaggregated (RoleCapacities == nil): one synthetic role "both" using
//     the engine-calibrated model-level RC/SC (Result.RequiredCapacity / SpareCapacity).
//     No re-aggregation — the engine already summed all variants into those scalars.
//
// Returns the list of active roles and the picker-local RolePairedState.
// Remaining/Spare scalars on NamedAnalyzerResult are read-only after this call;
// all dynamic bookkeeping moves to pickerState (scale-up) and RoleSpare (scale-down).
func initRoleState(s []NamedAnalyzerResult) (roles []string, pickerState RolePairedState) {
	pickerState = make(RolePairedState, len(s))
	roleSet := make(map[string]struct{})

	for i, e := range s {
		pickerState[i] = make(map[string]float64)
		if e.Result == nil {
			continue
		}
		if e.Result.RoleCapacities != nil {
			// Disaggregated: per-role RC/SC from engine-calibrated RoleCapacities.
			if s[i].RoleSpare == nil {
				s[i].RoleSpare = make(map[string]float64, len(e.Result.RoleCapacities))
			}
			for role, rc := range e.Result.RoleCapacities {
				pickerState[i][role] = rc.RequiredCapacity
				s[i].RoleSpare[role] = rc.SpareCapacity
				roleSet[role] = struct{}{}
			}
		} else {
			// Non-disaggregated: synthesize a single "both" role from model-level scalars.
			pickerState[i][interfaces.RoleBoth] = e.Remaining
			if s[i].RoleSpare == nil {
				s[i].RoleSpare = make(map[string]float64, 1)
			}
			s[i].RoleSpare[interfaces.RoleBoth] = e.Spare
			roleSet[interfaces.RoleBoth] = struct{}{}
		}
	}

	roles = make([]string, 0, len(roleSet))
	for role := range roleSet {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	return roles, pickerState
}

// =============================================================================
// Paired helpers — role-generic scale-up and scale-down
// =============================================================================
//
// Design § Architecture/D: (model, role) is the unit of allocation math.
// Per-role sizing is independent, scoped to each role's picker-local demand.
// The joint-commit step bounds by the min-util role (the coupling constraint).
//
// RolePairedState holds picker-local per-role demand tracked during one
// model's allocation pass. Indexed as [analyzer-index][role] → remaining demand
// (in that role's own capacity units). Initialized from RoleCapacities[role].RC;
// decremented per joint commit. Lives only inside the allocation loop — not
// stored on NamedAnalyzerResult (per design A10).
type RolePairedState []map[string]float64

// roleBottleneckReplicas computes the cross-analyzer bottleneck replica count
// for variant v in a specific role. Returns max_i ceil(state[i][role] / PRC_i[v]).
func roleBottleneckReplicas(s []NamedAnalyzerResult, state RolePairedState, role, v string) int {
	max := 0
	for i, e := range s {
		if e.Result == nil {
			continue
		}
		prc := prcForVariant(e.Result, v)
		if prc <= 0 {
			continue
		}
		n := int(math.Ceil(state[i][role] / prc))
		if n > max {
			max = n
		}
	}
	return max
}

// roleAggRemaining returns max cross-analyzer remaining demand for role.
func roleAggRemaining(s []NamedAnalyzerResult, state RolePairedState, role string) float64 {
	max := 0.0
	for i := range s {
		if d := state[i][role]; d > max {
			max = d
		}
	}
	return max
}

// anyRoleNeedsScaleUp is the per-role scale-up gate for the unified dispatcher.
// Returns true when any role has aggregate remaining demand > 0.
func anyRoleNeedsScaleUp(state RolePairedState, roles []string) bool {
	for _, role := range roles {
		for _, m := range state {
			if m[role] > 0 {
				return true
			}
		}
	}
	return false
}

// variantsForRole returns the capacities whose role matches role exactly,
// canonicalizing an empty Role to interfaces.RoleBoth.
func variantsForRole(vcs []interfaces.VariantCapacity, role string) []interfaces.VariantCapacity {
	out := make([]interfaces.VariantCapacity, 0, len(vcs))
	for _, vc := range vcs {
		r := vc.Role
		if r == "" {
			r = interfaces.RoleBoth
		}
		if r == role {
			out = append(out, vc)
		}
	}
	return out
}

// safeRemovalReplicasForRole returns the number of replicas of variant v that
// can safely be removed — the minimum of floor(RoleSpare[role]_i / PRC_i[v])
// across analyzers that have variant v and a non-zero PRC. Returns 0 if any
// contributing analyzer has RoleSpare[role] ≤ 0 or RoleSpare is nil.
func safeRemovalReplicasForRole(s []NamedAnalyzerResult, v, role string) int {
	smallest := math.MaxInt
	found := false
	for _, e := range s {
		if e.Result == nil || e.RoleSpare == nil {
			continue
		}
		prc := prcForVariant(e.Result, v)
		if prc <= 0 {
			continue
		}
		n := int(math.Floor(e.RoleSpare[role] / prc))
		if n < smallest {
			smallest = n
		}
		found = true
	}
	if !found || smallest < 0 {
		return 0
	}
	return smallest
}

// applyDeallocationForRole decrements each analyzer's RoleSpare[role] by
// n × PRC_i[v]. Clamps to 0. Never mutates Result.
func applyDeallocationForRole(s []NamedAnalyzerResult, v, role string, n int) {
	for i := range s {
		if s[i].Result == nil || s[i].RoleSpare == nil {
			continue
		}
		prc := prcForVariant(s[i].Result, v)
		if prc <= 0 {
			continue
		}
		s[i].RoleSpare[role] -= float64(n) * prc
		if s[i].RoleSpare[role] < 0 {
			s[i].RoleSpare[role] = 0
		}
	}
}

// needsScaleDownForRole reports whether every analyzer agrees this role has
// spare capacity (all-down gate, scoped to one role). Returns false if any
// analyzer's RoleSpare[role] ≤ 0 or RoleSpare is nil.
func needsScaleDownForRole(s []NamedAnalyzerResult, role string) bool {
	if len(s) == 0 {
		return false
	}
	for _, e := range s {
		if e.Result == nil || e.RoleSpare == nil {
			return false
		}
		if e.RoleSpare[role] <= 0 {
			return false
		}
	}
	return true
}

// RolePickFn is the role-generic optimizer variant selector for the unified
// allocateForModelPaired loop. Called once per role per iteration; returns the
// chosen variant and its resource cap. Returning ("", 0) signals no variant
// is available for this role.
type RolePickFn func(
	role string,
	s []NamedAnalyzerResult,
	variants []interfaces.VariantCapacity,
	stateMap map[string]interfaces.VariantReplicaState,
	available map[string]int,
	targets map[string]int,
) (variant string, capN int)

// allocateForModelPaired is the Phase-3 role-generic scale-up loop.
// Handles any set of roles (including the arity-1 "both" single-role case).
// Per iteration: pick one variant per role, size independently, compute
// Δ_util = min_role util_role, trim to matched joint commit.
// Arity-1 (roles = ["both"]) reduces to plain per-variant allocation.
func allocateForModelPaired(
	ctx context.Context,
	s []NamedAnalyzerResult,
	variants []interfaces.VariantCapacity,
	stateMap map[string]interfaces.VariantReplicaState,
	available map[string]int,
	targets map[string]int,
	pick RolePickFn,
	pickerState RolePairedState,
	roles []string,
) {
	logger := ctrl.LoggerFrom(ctx)
	for anyRoleNeedsScaleUp(pickerState, roles) {
		variantByRole := make(map[string]string, len(roles))
		capByRole := make(map[string]int, len(roles))
		prcByRole := make(map[string]float64, len(roles))
		allPicked := true
		for _, role := range roles {
			v, capN := pick(role, s, variants, stateMap, available, targets)
			if v == "" {
				allPicked = false
				break
			}
			variantByRole[role] = v
			capByRole[role] = capN
			prcByRole[role] = prcFromVCs(variants, v)
		}
		if !allPicked {
			break
		}

		nByRole := make(map[string]int, len(roles))
		utilByRole := make(map[string]float64, len(roles))
		for _, role := range roles {
			prc := prcByRole[role]
			n := min(roleBottleneckReplicas(s, pickerState, role, variantByRole[role]), capByRole[role])
			nByRole[role] = n
			demand := roleAggRemaining(s, pickerState, role)
			if demand <= 0 {
				utilByRole[role] = 1.0
			} else {
				utilByRole[role] = float64(n) * prc / demand
			}
		}

		deltaUtil := math.MaxFloat64
		for _, role := range roles {
			if utilByRole[role] < deltaUtil {
				deltaUtil = utilByRole[role]
			}
		}
		if deltaUtil <= 0 {
			break
		}

		kByRole := make(map[string]int, len(roles))
		anyPositive := false
		for _, role := range roles {
			demand := roleAggRemaining(s, pickerState, role)
			prc := prcByRole[role]
			n := nByRole[role]
			k := 0
			if prc > 0 && demand > 0 {
				k = max(int(math.Floor(deltaUtil*demand/prc)), min(1, n))
			}
			kByRole[role] = k
			if k > 0 {
				anyPositive = true
			}
		}
		if !anyPositive {
			break
		}

		for _, role := range roles {
			v := variantByRole[role]
			k := kByRole[role]
			prc := prcByRole[role]
			targets[v] += k
			for i := range pickerState {
				pickerState[i][role] = math.Max(0, pickerState[i][role]-float64(k)*prc)
			}
			if available != nil {
				available[accFromVCs(variants, v)] -= k * gpusPerReplicaFromState(stateMap, v)
			}
		}
		// Update model-level Remaining via the P-anchor role so fairShareValue
		// reflects committed capacity. For "both" (non-disaggregated) use the
		// single role; for P/D prefer "prefill".
		for _, anchor := range []string{"prefill", interfaces.RoleBoth} {
			if v, ok := variantByRole[anchor]; ok {
				applyAllocation(s, v, kByRole[anchor])
				break
			}
		}
		logger.V(logging.DEBUG).Info("scale-up: joint role commit", "deltaUtil", deltaUtil)
	}
}
