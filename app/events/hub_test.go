package events

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func supportPayload(userID string) []byte {
	payload, _ := json.Marshal(SupportEvent{Type: "thread.updated", ThreadID: "thread-" + userID, UserID: userID})
	return payload
}

// TestSupportHubRoutesByUser: a customer's connection gets only their own
// events; an admin connection (Subscribe) gets every event.
func TestSupportHubRoutesByUser(test *testing.T) {
	hub := NewSupportHub(nil)
	ada := hub.SubscribeUser("ada")
	bola := hub.SubscribeUser("bola")
	admin := hub.Subscribe()

	hub.broadcast(supportPayload("ada"))

	if got := len(ada); got != 1 {
		test.Fatalf("ada got %d events, want 1", got)
	}
	if got := len(bola); got != 0 {
		test.Fatalf("bola got %d events, want 0 (not her thread)", got)
	}
	if got := len(admin); got != 1 {
		test.Fatalf("admin got %d events, want 1", got)
	}
	if count := hub.SubscriberCount(); count != 3 {
		test.Fatalf("SubscriberCount = %d, want 3", count)
	}
	hub.UnsubscribeUser("ada", ada)
	hub.broadcast(supportPayload("ada"))
	if got := len(ada); got != 1 {
		test.Fatalf("ada still received after unsubscribing (%d events)", got)
	}
	if _, left := hub.byUser["ada"]; left {
		test.Fatal("an empty user entry was left behind")
	}
}

// TestRateHubPreparesFramesOnce: every connection receives the same
// ready-made SSE frame, and the newest raw event is kept for snapshots.
func TestRateHubPreparesFramesOnce(test *testing.T) {
	hub := NewRateHub(nil)
	first, second := hub.Subscribe(), hub.Subscribe()
	if hub.Latest() != nil {
		test.Fatal("Latest should be empty before any event")
	}
	event := []byte(`{"version":42,"rates":{}}`)
	hub.broadcast(event)

	frameA, frameB := <-first, <-second
	want := "event: rates.updated\nid: 42\ndata: {\"version\":42,\"rates\":{}}\n\n"
	if string(frameA) != want {
		test.Fatalf("frame = %q, want %q", frameA, want)
	}
	if &frameA[0] != &frameB[0] {
		test.Fatal("each connection got its own copy: the frame should be built once and shared")
	}
	if string(hub.Latest()) != string(event) {
		test.Fatalf("Latest = %s, want the raw event", hub.Latest())
	}
	if snapshot := string(RateFrame("rates.snapshot", event)); !strings.HasPrefix(snapshot, "event: rates.snapshot\nid: 42\n") {
		test.Fatalf("snapshot frame = %q", snapshot)
	}
}

// TestSlowConnectionKeepsNewest: a full buffer drops the oldest event, never blocks.
func TestSlowConnectionKeepsNewest(test *testing.T) {
	hub := NewRateHub(nil) // buffer of 1
	slow := hub.Subscribe()
	hub.broadcast([]byte(`{"version":1}`))
	hub.broadcast([]byte(`{"version":2}`))
	if frame := string(<-slow); !strings.Contains(frame, "id: 2") {
		test.Fatalf("slow connection kept %q, want the newest (version 2)", frame)
	}
}

// BenchmarkSupportBroadcast1000Customers: one support event with 1,000
// customers connected. Routing by user decodes it once and touches only the
// owner's socket (before, all 1,000 sockets decoded it).
func BenchmarkSupportBroadcast1000Customers(benchmark *testing.B) {
	hub := NewSupportHub(nil)
	for index := 0; index < 1000; index++ {
		connection := hub.SubscribeUser(fmt.Sprintf("user-%d", index))
		go func() {
			for range connection {
			}
		}()
	}
	payload := supportPayload("user-500")
	benchmark.ResetTimer()
	for index := 0; index < benchmark.N; index++ {
		hub.broadcast(payload)
	}
}
