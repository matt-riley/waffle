package dashboard

import (
	"context"
	"testing"
	"time"
)

func TestEventHubRequiresResyncAfterCursorFallsBehind(t *testing.T) {
	hub := NewEventHub(2)
	hub.Publish(Event{Type: "one"})
	hub.Publish(Event{Type: "two"})
	hub.Publish(Event{Type: "three"})
	_, resync := hub.Subscribe(0)
	if !resync {
		t.Fatal("old cursor must require resync")
	}
}

func TestEventHubReplaysRetainedEventsInOrderAndKeepsLiveSubscription(t *testing.T) {
	hub := NewEventHub(3)
	first := hub.Publish(Event{Cursor: 999, Type: "one"})
	second := hub.Publish(Event{Type: "two"})
	third := hub.Publish(Event{Type: "three"})
	if first.Cursor != 1 || second.Cursor != 2 || third.Cursor != 3 {
		t.Fatalf("cursors = %d, %d, %d, want 1, 2, 3", first.Cursor, second.Cursor, third.Cursor)
	}

	subscription, resync := hub.Subscribe(first.Cursor)
	if resync {
		t.Fatal("retained cursor unexpectedly requires resync")
	}
	for want := uint64(2); want <= 3; want++ {
		select {
		case event := <-subscription:
			if event.Cursor != want {
				t.Fatalf("replayed cursor = %d, want %d", event.Cursor, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for replay cursor %d", want)
		}
	}

	hub.Publish(Event{Type: "live"})
	select {
	case event := <-subscription:
		if event.Cursor != 4 || event.Type != "live" {
			t.Fatalf("live event = %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for live event")
	}
	hub.Unsubscribe(subscription)
}

func TestEventHubCurrentCursorProvidesOpenLiveSubscription(t *testing.T) {
	hub := NewEventHub(2)
	current := hub.Publish(Event{Type: "one"})
	subscription, resync := hub.Subscribe(current.Cursor)
	if resync {
		t.Fatal("current cursor unexpectedly requires resync")
	}
	select {
	case event, open := <-subscription:
		t.Fatalf("unexpected replay event %#v (open=%t)", event, open)
	default:
	}
	hub.Unsubscribe(subscription)
}

func TestEventHubEvictsSlowSubscriberWithoutBlockingPublisher(t *testing.T) {
	hub := NewEventHub(64)
	slow, resync := hub.Subscribe(0)
	if resync {
		t.Fatal("empty hub must accept cursor zero")
	}
	for i := 0; i < 32; i++ {
		hub.Publish(Event{Type: "fill"})
	}
	published := make(chan struct{})
	go func() {
		hub.Publish(Event{Type: "evict"})
		close(published)
	}()
	select {
	case <-published:
	case <-time.After(time.Second):
		t.Fatal("publishing to a slow subscriber blocked")
	}
	for range slow {
	}
}

func TestEventHubUnsubscribeIsSafeDuringPublication(t *testing.T) {
	hub := NewEventHub(2)
	subscription, resync := hub.Subscribe(0)
	if resync {
		t.Fatal("empty hub must accept cursor zero")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-ctx.Done():
				return
			default:
				hub.Publish(Event{Type: "race"})
			}
		}
	}()
	hub.Unsubscribe(subscription)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("publisher did not finish")
	}
}
