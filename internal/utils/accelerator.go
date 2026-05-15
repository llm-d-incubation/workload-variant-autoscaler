package utils

import "strings"

// NormalizeAcceleratorName converts a full GPU model name to a short name.
// This enables matching between VA labels (e.g., "A100") and discovery results
// (e.g., "NVIDIA-A100-PCIE-80GB").
//
// Examples:
//   - "NVIDIA-A100-PCIE-80GB" -> "A100"
//   - "NVIDIA-H100-SXM5-80GB" -> "H100"
//   - "AMD-MI300X-192G" -> "MI300X"
//   - "Intel-Gaudi-2-96GB" -> "Gaudi-2"
//   - "A100" -> "A100" (already short)
func NormalizeAcceleratorName(fullName string) string {
	// If already a short name (no hyphens or known pattern), return as-is
	if !strings.Contains(fullName, "-") {
		return fullName
	}

	// Common patterns for GPU model names:
	// NVIDIA-{model}-{variant} -> extract {model}
	// AMD-{model}-{memory} -> extract {model}
	// Intel-{model}-{memory} -> extract {model}

	parts := strings.Split(fullName, "-")
	if len(parts) < 2 {
		return fullName
	}

	// Check for known vendor prefixes
	vendor := strings.ToUpper(parts[0])
	switch vendor {
	case "NVIDIA":
		// NVIDIA-A100-PCIE-80GB -> A100
		// NVIDIA-H100-SXM5-80GB -> H100
		if len(parts) >= 2 {
			return parts[1]
		}
	case "AMD":
		// AMD-MI300X-192G -> MI300X
		if len(parts) >= 2 {
			return parts[1]
		}
	case "INTEL":
		// Intel-Gaudi-2-96GB -> Gaudi-2
		if len(parts) >= 3 {
			return parts[1] + "-" + parts[2]
		}
		if len(parts) >= 2 {
			return parts[1]
		}
	}

	// Fallback: return the second part (after vendor)
	return parts[1]
}
