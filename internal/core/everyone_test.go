package core

import (
	"testing"

	"github.com/wallter/a2p2/internal/model/delivery"
)

// Requirements: U1. The "@everyone" mark is the delivery model's rule.
func TestMarkedForEveryoneMatchesTheDeliveryModel(t *testing.T) {
	for _, text := range []string{
		"@everyone", "@everyone please read", "@EVERYONE: stop", "@Everyone, hi", "@everyone\nnext",
		"@everyone-wide", "@everyone x", "@everyone。", "@everyones", "@everyone2", "@everyone_x",
		" @everyone", "hi @everyone", "everyone", "@every", "", "@everyoné", "@everyone+1",
	} {
		want := delivery.MarkedForEveryone(text)
		if got := MarkedForEveryone(text); got != want {
			t.Errorf("MarkedForEveryone(%q) = %v, the model says %v", text, got, want)
		}
	}
	if !MarkedForEveryone("@everyone look") || MarkedForEveryone("look @everyone") {
		t.Fatal("the mark is the start of the text")
	}
}
