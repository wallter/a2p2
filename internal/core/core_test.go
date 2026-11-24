package core

import (
	"strings"
	"testing"
)

func TestCleanLabel(t *testing.T) {
	for in, want := range map[string]string{"A2P2": "A2P2", "my project!": "my-project", "///": "agent", "": "agent"} {
		if got := CleanLabel(in); got != want || !ValidLabel(got) {
			t.Errorf("CleanLabel(%q) = %q, want %q", in, got, want)
		}
	}
	if got := CleanLabel(strings.Repeat("x", 200)); !ValidLabel(got + "-99") {
		t.Errorf("long label leaves no room for suffix: %d chars", len(got))
	}
}
