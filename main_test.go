package main

import (
	"reflect"
	"testing"
)

func TestNormalizeCountries(t *testing.T) {
	tests := []struct {
		name     string
		input    []string
		expected []string
	}{
		{
			name:     "single country",
			input:    []string{"ca"},
			expected: []string{"CA"},
		},
		{
			name:     "comma separated",
			input:    []string{"US,CA"},
			expected: []string{"US", "CA"},
		},
		{
			name:     "mixed separators",
			input:    []string{"US, CA", "gb ie"},
			expected: []string{"US", "CA", "GB", "IE"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			actual := NormalizeCountries(tc.input)
			if !reflect.DeepEqual(actual, tc.expected) {
				t.Fatalf("NormalizeCountries(%v) = %v, want %v", tc.input, actual, tc.expected)
			}
		})
	}
}
