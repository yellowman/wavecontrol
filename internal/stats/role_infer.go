package stats

import "strings"

// RoleInference describes an inferred device role.
//
// Role is one of:
//   - "ap"
//   - "sta"
//   - "" (unknown)
//
// Source is a short hint about what drove the inference.
type RoleInference struct {
	Role   string
	Source string
}

// NormalizeRole attempts to normalize common role strings into "ap" / "sta".
// Returns "" if the input can't be mapped.
func NormalizeRole(s string) string {
	v := strings.ToLower(strings.TrimSpace(s))
	switch v {
	case "ap", "accesspoint", "access_point", "access-point":
		return "ap"
	case "sta", "station", "client", "cpe":
		return "sta"
	default:
		// Some firmware uses values like "ap-ptp" / "ap-ptmp".
		if strings.HasPrefix(v, "ap") {
			return "ap"
		}
		if strings.HasPrefix(v, "sta") {
			return "sta"
		}
		return ""
	}
}

// InferRoleFromIdentity tries to infer AP vs STA from identity-ish fields
// (platform/product/flavor). This is intentionally conservative; it only
// returns a role when it is strongly implied by the hardware/product line.
func InferRoleFromIdentity(platform, product, flavor string) RoleInference {
	p := strings.ToLower(strings.TrimSpace(platform))
	prod := strings.ToLower(strings.TrimSpace(product))
	flav := strings.ToLower(strings.TrimSpace(flavor))

	// LTU: Rocket is (effectively) always an AP; XR/LR/Lite are CPE/STA.
	if p == "ltu" {
		if strings.Contains(prod, "rocket") || strings.Contains(flav, "rocket") {
			return RoleInference{Role: "ap", Source: "ltu-product"}
		}
		// Common LTU CPE models.
		if strings.Contains(prod, "xr") || strings.Contains(prod, "lr") || strings.Contains(prod, "lite") {
			return RoleInference{Role: "sta", Source: "ltu-product"}
		}
		return RoleInference{}
	}

	// Wave: "Wave AP" denotes the base station. Other Wave devices are typically stations.
	if p == "wave" {
		if strings.Contains(prod, "wave ap") || strings.Contains(prod, "waveap") || strings.Contains(flav, "waveap") {
			return RoleInference{Role: "ap", Source: "wave-product"}
		}
		if strings.Contains(prod, "wave") || strings.Contains(flav, "wave") {
			return RoleInference{Role: "sta", Source: "wave-product"}
		}
		return RoleInference{}
	}

	// AirMax / AirFiber / etc can often be reconfigured AP<->STA, so identity inference is unreliable.
	return RoleInference{}
}
