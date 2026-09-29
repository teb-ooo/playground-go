package uuidv7

import (
	"sort"
	"testing"
)

func TestNewIsValidAndMonotonic(t *testing.T) {
	const n = 5000
	ids := make([]string, n)
	for i := range ids {
		ids[i] = New()
		if !Valid(ids[i]) {
			t.Fatalf("invalid uuidv7 %q", ids[i])
		}
	}
	if !sort.StringsAreSorted(ids) {
		t.Fatal("ids not monotonically increasing")
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			t.Fatalf("duplicate %s", id)
		}
		seen[id] = true
	}
}

func TestValid(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b", true},
		{"0190a1b2-c3d4-4e5f-8a9b-0c1d2e3f4a5b", false},
		{"nope", false},
		{"0190a1b2c3d47e5f8a9b0c1d2e3f4a5b----", false},
		{"", false},
	}
	for _, tc := range tests {
		if got := Valid(tc.in); got != tc.want {
			t.Errorf("Valid(%q)=%v want %v", tc.in, got, tc.want)
		}
	}
}
