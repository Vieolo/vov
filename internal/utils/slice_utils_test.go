package utils

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRequestPolicySliceAllCheck(t *testing.T) {
	assert.True(t, RequestSliceSatisfiesPolicy([]string{}, []string{}))
	assert.True(t, RequestSliceSatisfiesPolicy([]string{"one"}, []string{}))
	assert.True(t, RequestSliceSatisfiesPolicy([]string{"one"}, []string{"one"}))
	assert.True(t, RequestSliceSatisfiesPolicy([]string{"one", "two", "three"}, []string{"one", "two"}))

	assert.False(t, RequestSliceSatisfiesPolicy([]string{}, []string{"one"}))
	assert.False(t, RequestSliceSatisfiesPolicy([]string{"one"}, []string{"two"}))
	assert.False(t, RequestSliceSatisfiesPolicy([]string{"one"}, []string{"one", "two"}))
	assert.False(t, RequestSliceSatisfiesPolicy([]string{"one", "four", "three"}, []string{"one", "two"}))
}

func TestRequestPolicySliceAnyCheck(t *testing.T) {
	assert.True(t, RequestSliceSatisfiesAnyPolicy([]string{}, []string{}))
	assert.True(t, RequestSliceSatisfiesAnyPolicy([]string{"one"}, []string{}))
	assert.True(t, RequestSliceSatisfiesAnyPolicy([]string{"one"}, []string{"one"}))
	assert.True(t, RequestSliceSatisfiesAnyPolicy([]string{"one"}, []string{"two", "one"}))
	assert.True(t, RequestSliceSatisfiesAnyPolicy([]string{"one", "two"}, []string{"two"}))

	assert.False(t, RequestSliceSatisfiesAnyPolicy([]string{}, []string{"one"}))
	assert.False(t, RequestSliceSatisfiesAnyPolicy([]string{"one"}, []string{"two"}))
	assert.False(t, RequestSliceSatisfiesAnyPolicy([]string{"one", "two"}, []string{"three", "four"}))
}
