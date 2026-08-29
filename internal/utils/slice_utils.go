package utils

import "slices"

// Checks if the request slice has all the values required by the policy
//
// Can be used for scope, permission, and roles
func RequestSliceSatisfiesPolicy(requestSlice, policySlice []string) bool {
	if len(requestSlice) < len(policySlice) {
		return false
	}

	for _, pol := range policySlice {
		if !slices.Contains(requestSlice, pol) {
			return false
		}
	}
	return true
}
