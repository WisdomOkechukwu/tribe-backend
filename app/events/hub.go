// Package events pushes live updates to connected phones and admin screens.
//
// A Hub listens to ONE Redis pub/sub channel and forwards every message to
// every open SSE connection. There are two hubs:
//
//	RateHub     "rates.updated"   -> phones on rate screens   (keeps only the newest event)
//	SupportHub  "support.events"  -> admins on the inbox page (SSE) and customers
//	                                 in a support chat (WebSocket, own events only)
//
// The rate hub in one picture:
//
//	scheduler ──publish──> Redis "rates.updated" ──> RateHub goroutine (one per API server)
//	                                                       │
//	                         ┌─────────────────────────────┼──────────────────────┐
//	                         ▼                             ▼                      ▼
//	                 phone on swap page          phone on send page      phone on gift page
//	                 (SSE connection)            (SSE connection)        (SSE connection)
//
// A "goroutine" is a function running at the same time as everything else
// (like a helper working in the background). A "channel" is a pipe that
// goroutines use to pass messages to each other safely.
package events

import (
	"context"
	"encoding/json"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/tribe-africa/tribe-backend/app/services/rates"
)

// ChannelSupportEvents is where the API publishes "a support thread changed".
const ChannelSupportEvents = "support.events"

// SupportEvent is one "a support thread changed" message. The admin inbox
// reloads the thread; the customer's chat socket only forwards events whose
// UserID is the signed-in customer (see the mobile SupportSocket).
type SupportEvent struct {
	Type     string `json:"type"` // ticket.new | chat.message | thread.updated | thread.status
	ThreadID string `json:"thread_id"`
	UserID   string `json:"user_id"`
}

// PublishSupportEvent sends a SupportEvent to every API server through Redis.
func PublishSupportEvent(requestContext context.Context, redisClient *redis.Client, eventType string, threadID string, userID string) {
	payload, _ := json.Marshal(SupportEvent{Type: eventType, ThreadID: threadID, UserID: userID})
	redisClient.Publish(requestContext, ChannelSupportEvents, payload)
}

// Hub keeps the list of open connections and forwards every event to them.
//
// Work is done ONCE per event, not once per connection: `prepare` turns the
// Redis message into exactly the bytes a connection writes (the rate hub
// builds the whole SSE frame), and `route` reads which customer an event is
// for so it goes only to that customer's connections (the support hub). With
// 1,000 phones connected, an event is decoded once instead of 1,000 times.
type Hub struct {
	redis       *redis.Client
	channel     string               // the Redis pub/sub channel this hub listens to
	bufferSize  int                  // events kept per connection before the oldest is dropped
	mutex       sync.Mutex           // protects everything below (many goroutines touch it)
	subscribers map[chan []byte]bool // connections that get EVERY event (admin inbox, rate screens)
	// connections that get only one customer's events (customer chat sockets), by user id
	byUser map[string]map[chan []byte]bool
	// optional, run once per event: the bytes every connection receives (nil = the message as is)
	prepare func(payload []byte) []byte
	// optional, run once per event: the customer it belongs to ("" = nobody in particular)
	route func(payload []byte) string
	// the newest raw message, kept for connections that open later (the rate snapshot)
	keepLatest bool
	latest     []byte
}

// NewRateHub creates the hub for live rates. A slow phone only needs the
// NEWEST rates, so each connection keeps just one event. Each update is
// turned into a ready-to-write SSE frame once, and the newest one is kept so
// a new connection's snapshot needs no Redis or database read.
func NewRateHub(redisClient *redis.Client) *Hub {
	return &Hub{
		redis: redisClient, channel: rates.ChannelRatesUpdated, bufferSize: 1,
		subscribers: map[chan []byte]bool{}, byUser: map[string]map[chan []byte]bool{},
		prepare: func(payload []byte) []byte { return RateFrame("rates.updated", payload) }, keepLatest: true,
	}
}

// RateFrame builds one SSE frame for a rates event: the event name, its
// version as the SSE id (when it has one) and the JSON as data.
func RateFrame(eventName string, payload []byte) []byte {
	var head struct {
		Version uint64 `json:"version"`
	}
	json.Unmarshal(payload, &head)
	frame := make([]byte, 0, len(payload)+48)
	frame = append(frame, "event: "...)
	frame = append(frame, eventName...)
	frame = append(frame, "\nid: "...)
	frame = strconv.AppendUint(frame, head.Version, 10)
	frame = append(frame, "\ndata: "...)
	frame = append(frame, payload...)
	return append(frame, "\n\n"...)
}

// NewSupportHub creates the hub for support. Admins subscribe to everything;
// customer chat sockets subscribe to their own user id and get only their
// events (the user id is read once per event here, not in every socket).
// Each event names a different thread, so a connection keeps up to 32.
func NewSupportHub(redisClient *redis.Client) *Hub {
	return &Hub{
		redis: redisClient, channel: ChannelSupportEvents, bufferSize: 32,
		subscribers: map[chan []byte]bool{}, byUser: map[string]map[chan []byte]bool{},
		route: func(payload []byte) string {
			var event SupportEvent
			if json.Unmarshal(payload, &event) != nil {
				return ""
			}
			return event.UserID
		},
	}
}

// Run listens to Redis until the context is cancelled. Start it ONCE with `go hub.Run(ctx)`.
// If Redis drops, it waits a second and listens again.
func (hub *Hub) Run(requestContext context.Context) {
	for requestContext.Err() == nil {
		subscription := hub.redis.Subscribe(requestContext, hub.channel)
		messages := subscription.Channel()
		for message := range messages {
			hub.broadcast([]byte(message.Payload))
		}
		subscription.Close()
		select {
		case <-requestContext.Done():
			return
		case <-time.After(time.Second):
			slog.Warn("event hub reconnecting to Redis", "channel", hub.channel)
		}
	}
}

// Subscribe registers a connection that gets every event. Always call
// Unsubscribe when the connection closes.
func (hub *Hub) Subscribe() chan []byte {
	// If a connection is slow, its buffer fills and the OLDEST event is
	// dropped to make room (see deliver), so the API never waits for it.
	connection := make(chan []byte, hub.bufferSize)
	hub.mutex.Lock()
	hub.subscribers[connection] = true
	hub.mutex.Unlock()
	return connection
}

// Unsubscribe removes a connection made with Subscribe.
func (hub *Hub) Unsubscribe(connection chan []byte) {
	hub.mutex.Lock()
	delete(hub.subscribers, connection)
	hub.mutex.Unlock()
}

// SubscribeUser registers a connection that gets only the events routed to
// this user (one customer can have several, e.g. after reconnecting). Always
// call UnsubscribeUser when it closes.
func (hub *Hub) SubscribeUser(userID string) chan []byte {
	connection := make(chan []byte, hub.bufferSize)
	hub.mutex.Lock()
	if hub.byUser[userID] == nil {
		hub.byUser[userID] = map[chan []byte]bool{}
	}
	hub.byUser[userID][connection] = true
	hub.mutex.Unlock()
	return connection
}

// UnsubscribeUser removes a connection made with SubscribeUser.
func (hub *Hub) UnsubscribeUser(userID string, connection chan []byte) {
	hub.mutex.Lock()
	delete(hub.byUser[userID], connection)
	if len(hub.byUser[userID]) == 0 {
		delete(hub.byUser, userID)
	}
	hub.mutex.Unlock()
}

// Latest returns the newest raw message (rate hub only), or nil before the first one.
func (hub *Hub) Latest() []byte {
	hub.mutex.Lock()
	defer hub.mutex.Unlock()
	return hub.latest
}

// SubscriberCount returns how many connections are listening (shown on /health).
func (hub *Hub) SubscriberCount() int {
	hub.mutex.Lock()
	defer hub.mutex.Unlock()
	count := len(hub.subscribers)
	for _, connections := range hub.byUser {
		count += len(connections)
	}
	return count
}

// broadcast prepares an event once, then hands it to every connection that
// should see it, without ever blocking. The decoding (prepare, route) happens
// before the lock is taken, so subscribing never waits on JSON work.
func (hub *Hub) broadcast(event []byte) {
	prepared := event
	if hub.prepare != nil {
		prepared = hub.prepare(event)
	}
	userID := ""
	if hub.route != nil {
		userID = hub.route(event)
	}
	hub.mutex.Lock()
	defer hub.mutex.Unlock()
	if hub.keepLatest {
		hub.latest = event
	}
	for connection := range hub.subscribers {
		deliver(connection, prepared)
	}
	if userID != "" {
		for connection := range hub.byUser[userID] {
			deliver(connection, prepared)
		}
	}
}

// deliver puts an event on one connection: if it still has an unread event
// and its buffer is full, the oldest is dropped to make room for the new one.
func deliver(connection chan []byte, event []byte) {
	select {
	case connection <- event:
	default:
		select {
		case <-connection:
		default:
		}
		select {
		case connection <- event:
		default:
		}
	}
}
