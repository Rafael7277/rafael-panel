package model

import "math"

// MaxClientQuotaFactor bounds the divisor accepted from panel/API inputs.
// A zero factor means no scaling and is normalized to 1 for calculations.
const MaxClientQuotaFactor int64 = 10000

// ClientQuotaFactor returns the configured divisor, treating a missing or
// zero factor as the legacy behavior (no scaling).
func ClientQuotaFactor(factor *int64) int64 {
	if factor == nil || *factor <= 1 {
		return 1
	}
	return *factor
}

// InitialClientTrafficQuota returns the effective byte allowance for a fresh
// traffic cycle. The nominal quota remains stored on Client.TotalGB.
func InitialClientTrafficQuota(client Client) int64 {
	if client.TotalGB <= 0 {
		return 0
	}
	quota := client.TotalGB / ClientQuotaFactor(client.QuotaFactor)
	if quota == 0 {
		return 1
	}
	return quota
}

// UpdatedClientTrafficQuota applies only the nominal quota delta to the
// currently stored effective allowance. A factor edit by itself therefore
// never recalculates already granted traffic. A transition from unlimited to
// finite starts a new allocation using the new factor.
func UpdatedClientTrafficQuota(oldClient, newClient Client, currentEffective int64) int64 {
	if newClient.TotalGB <= 0 {
		return 0
	}
	if oldClient.TotalGB <= 0 {
		return InitialClientTrafficQuota(newClient)
	}
	if currentEffective <= 0 {
		currentEffective = InitialClientTrafficQuota(oldClient)
	}
	factor := newClient.QuotaFactor
	if factor == nil {
		factor = oldClient.QuotaFactor
	}
	delta := newClient.TotalGB - oldClient.TotalGB
	scaledDelta := delta / ClientQuotaFactor(factor)
	if delta < 0 && delta%ClientQuotaFactor(factor) != 0 {
		// Round a reduction away from zero so lowering a quota cannot leave
		// extra effective traffic due to integer truncation.
		scaledDelta--
	}
	if scaledDelta > 0 && currentEffective > math.MaxInt64-scaledDelta {
		return math.MaxInt64
	}
	if scaledDelta < 0 && currentEffective < -scaledDelta {
		return 0
	}
	return currentEffective + scaledDelta
}

// ValidateClientQuotaFactor rejects negative or unreasonable divisors.
func ValidateClientQuotaFactor(factor *int64) bool {
	return factor == nil || (*factor >= 0 && *factor <= MaxClientQuotaFactor)
}
