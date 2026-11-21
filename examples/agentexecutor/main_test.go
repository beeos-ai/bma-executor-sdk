package main

import (
	"context"
	"testing"
	"time"
)

// TestSimulation runs the documented flow end to end: task, streamed events,
// approval, result, follow-up, input, network drop and resend, cancel, close.
func TestSimulation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := simulate(ctx); err != nil {
		t.Fatal(err)
	}
}
