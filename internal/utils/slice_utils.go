package utils

import "slices"

// Checks if the request slice has all the values required by the policy
//
// An empty policy places no requirement
func RequestSliceSatisfiesPolicy(requestSlice, policySlice []string) bool {
	if len(policySlice) == 0 {
		return true
	}
	for _, pol := range policySlice {
		if !slices.Contains(requestSlice, pol) {
			return false
		}
	}
	return true
}

// Checks if the request slice has at least one of the values listed by the policy
//
// An empty policy places no requirement
func RequestSliceSatisfiesAnyPolicy(requestSlice, policySlice []string) bool {
	if len(policySlice) == 0 {
		return true
	}
	for _, pol := range policySlice {
		if slices.Contains(requestSlice, pol) {
			return true
		}
	}
	return false
}
