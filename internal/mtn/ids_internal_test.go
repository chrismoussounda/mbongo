package mtn

import "testing"

func TestNewUUIDIsAVersion4UUID(t *testing.T) {
	seen := map[string]bool{}
	for range 1000 {
		id := newUUID()
		if !isUUID(id) || seen[id] {
			t.Fatalf("%q is not a fresh UUID v4", id)
		}
		seen[id] = true
	}
}
