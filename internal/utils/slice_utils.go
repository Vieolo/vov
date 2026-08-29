package utils

import "slices"

// Checks if the request slice has all the values required by the policy
func RequestSliceSatisfiesPolicy(requestSlice, policySlice []string) bool {
	for _, pol := range policySlice {
		if !slices.Contains(requestSlice, pol) {
			return false
		}
	}
	return true
}
