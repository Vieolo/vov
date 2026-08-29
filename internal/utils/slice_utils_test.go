package utils

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRequestPolicySliceCheck(t *testing.T) {
	assert.True(t, RequestSliceSatisfiesPolicy([]string{}, []string{}))
	assert.True(t, RequestSliceSatisfiesPolicy([]string{"one"}, []string{}))
	assert.True(t, RequestSliceSatisfiesPolicy([]string{"one"}, []string{"one"}))
	assert.True(t, RequestSliceSatisfiesPolicy([]string{"one", "two", "three"}, []string{"one", "two"}))

	assert.False(t, RequestSliceSatisfiesPolicy([]string{}, []string{"one"}))
	assert.False(t, RequestSliceSatisfiesPolicy([]string{"one"}, []string{"two"}))
	assert.False(t, RequestSliceSatisfiesPolicy([]string{"one"}, []string{"one", "two"}))
	assert.False(t, RequestSliceSatisfiesPolicy([]string{"one", "four", "three"}, []string{"one", "two"}))
}
