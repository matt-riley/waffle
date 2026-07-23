package dashboard

import (
	"encoding/json"
	"sync"
)

const subscriberBufferSize = 32

// Event is a sanitized change notification emitted to connected Desk clients.
type Event struct {
	Cursor     uint64          `json:"cursor"`
	Type       string          `json:"type"`
	Resource   string          `json:"resource,omitempty"`
	ResourceID string          `json:"resource_id,omitempty"`
	Data       json.RawMessage `json:"data,omitempty"`
}

// EventHub retains a bounded replay history and fan-outs events without ever
// allowing a slow subscriber to block a publisher.
type EventHub struct {
	mu          sync.Mutex
	capacity    int
	next        uint64
	ring        []Event
	subscribers map[chan Event]struct{}
}

// NewEventHub returns a bounded, concurrency-safe event hub. A non-positive
// requested capacity is normalized to one retained event.
func NewEventHub(capacity int) *EventHub {
	if capacity <= 0 {
		capacity = 1
	}
	return &EventHub{
		capacity:    capacity,
		ring:        make([]Event, 0, capacity),
		subscribers: make(map[chan Event]struct{}),
	}
}

// Publish assigns the next cursor, retains the event, and notifies each live
// subscriber. Subscribers that cannot keep up are disconnected.
func (h *EventHub) Publish(event Event) Event {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.next++
	event.Cursor = h.next
	if len(h.ring) == h.capacity {
		copy(h.ring, h.ring[1:])
		h.ring[len(h.ring)-1] = event
	} else {
		h.ring = append(h.ring, event)
	}
	for subscriber := range h.subscribers {
		select {
		case subscriber <- event:
		default:
			delete(h.subscribers, subscriber)
			close(subscriber)
		}
	}
	return event
}

// Subscribe replays retained events after after and then remains subscribed for
// live events. resync is true when the requested cursor is no longer retained.
func (h *EventHub) Subscribe(after uint64) (<-chan Event, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if after > h.next {
		return nil, true
	}
	start := 0
	if len(h.ring) > 0 {
		oldest := h.ring[0].Cursor
		if after < oldest-1 {
			return nil, true
		}
		for start < len(h.ring) && h.ring[start].Cursor <= after {
			start++
		}
	}
	if len(h.ring)-start > subscriberBufferSize {
		return nil, true
	}

	subscriber := make(chan Event, subscriberBufferSize)
	for _, event := range h.ring[start:] {
		subscriber <- event
	}
	h.subscribers[subscriber] = struct{}{}
	return subscriber, false
}

// Unsubscribe removes and closes a subscription. It is safe to call more than
// once or concurrently with Publish.
func (h *EventHub) Unsubscribe(subscription <-chan Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for subscriber := range h.subscribers {
		if (<-chan Event)(subscriber) == subscription {
			delete(h.subscribers, subscriber)
			close(subscriber)
			return
		}
	}
}

// Cursor returns the most recently assigned event cursor.
func (h *EventHub) Cursor() uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.next
}
