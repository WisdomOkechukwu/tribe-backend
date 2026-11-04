package mobile

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"

	"github.com/tribe-africa/tribe-backend/app/bootstrap"
	"github.com/tribe-africa/tribe-backend/app/http/middleware"
)

// How the support socket keeps itself healthy.
//   - supportPingEvery: the server pings this often; phones answer with a pong automatically.
//   - supportPongWait: no pong (or message) for this long and the connection is treated as dead.
//   - supportWriteWait: one frame must be written within this time, or the connection is dropped.
const (
	supportPingEvery = 25 * time.Second
	supportPongWait  = 60 * time.Second
	supportWriteWait = 10 * time.Second
)

// supportUpgrader turns the HTTP request into a WebSocket. Phones don't send
// an Origin header (that's a browser thing), and the request is already
// authenticated by the signedIn middleware, so every origin is accepted.
var supportUpgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin:     func(request *http.Request) bool { return true },
}

// SupportSocketController holds the customer's live support connection.
type SupportSocketController struct {
	application *bootstrap.App
}

// NewSupportSocketController creates the controller.
func NewSupportSocketController(application *bootstrap.App) *SupportSocketController {
	return &SupportSocketController{application: application}
}

// SupportSocket  GET /support/stream  -> a WebSocket with the customer's own support events.
//
// It subscribes to SupportHub by user id, so it receives only this
// customer's events (the hub reads the user id once per event, instead of
// every socket decoding every event), and forwards them as JSON text frames:
//
//	{"type":"thread.updated","thread_id":"01J...","user_id":"01J..."}
//
// "thread.updated" = an agent replied, "thread.status" = the status changed,
// "chat.message" / "ticket.new" = the customer wrote (so their other screens
// refresh too). The app then reloads that chat. Nothing is ever read from the
// phone except control frames: messages are still sent with POST, so they go
// through validation, idempotency and the audit like everything else.
func (controller *SupportSocketController) SupportSocket(httpContext *gin.Context) {
	userID := middleware.CurrentActor(httpContext).UserID
	connection, err := supportUpgrader.Upgrade(httpContext.Writer, httpContext.Request, nil)
	if err != nil {
		// Upgrade has already written the HTTP error for us.
		return
	}
	defer connection.Close()

	feed := controller.application.SupportHub.SubscribeUser(userID)
	defer controller.application.SupportHub.UnsubscribeUser(userID, feed)

	// The reader: a WebSocket must be read for pongs and the close frame to
	// be noticed. When the phone goes away, `closed` is closed and the writer
	// below stops. Anything else the phone sends is ignored.
	closed := make(chan struct{})
	connection.SetReadLimit(512)
	connection.SetReadDeadline(time.Now().Add(supportPongWait))
	connection.SetPongHandler(func(string) error {
		return connection.SetReadDeadline(time.Now().Add(supportPongWait))
	})
	go func() {
		defer close(closed)
		for {
			if _, _, err := connection.ReadMessage(); err != nil {
				return
			}
		}
	}()

	// The writer: one hello so the app knows it's live, then this customer's
	// events as they come, and a ping every 25 s so proxies keep it open.
	connection.SetWriteDeadline(time.Now().Add(supportWriteWait))
	if err := connection.WriteMessage(websocket.TextMessage, []byte(`{"type":"connected"}`)); err != nil {
		return
	}
	ping := time.NewTicker(supportPingEvery)
	defer ping.Stop()
	for {
		select {
		case <-closed:
			return
		case <-httpContext.Request.Context().Done():
			return
		case payload := <-feed:
			connection.SetWriteDeadline(time.Now().Add(supportWriteWait))
			if err := connection.WriteMessage(websocket.TextMessage, payload); err != nil {
				slog.Debug("support socket write failed", "error", err)
				return
			}
		case <-ping.C:
			connection.SetWriteDeadline(time.Now().Add(supportWriteWait))
			if err := connection.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}
