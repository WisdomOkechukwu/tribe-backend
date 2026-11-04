package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/tribe-africa/tribe-backend/tests/testenv"
)

// dialSupport opens a customer's support WebSocket (GET /support/stream) and
// waits for the server's {"type":"connected"} hello.
func dialSupport(test *testing.T, environment *testenv.Environment, client *testenv.Client) *websocket.Conn {
	test.Helper()
	url := "ws" + strings.TrimPrefix(environment.Server.URL, "http") + "/api/mobile/v1/support/stream"
	header := http.Header{}
	header.Set("Authorization", "Bearer "+client.Token)
	header.Set("X-Device-Imei", client.DeviceImei)
	connection, response, err := websocket.DefaultDialer.Dial(url, header)
	if err != nil {
		status := 0
		if response != nil {
			status = response.StatusCode
		}
		test.Fatalf("dial support socket: %v (HTTP %d)", err, status)
	}
	test.Cleanup(func() { connection.Close() })
	var hello struct {
		Type string `json:"type"`
	}
	connection.SetReadDeadline(time.Now().Add(5 * time.Second))
	if err := connection.ReadJSON(&hello); err != nil || hello.Type != "connected" {
		test.Fatalf("socket hello = %+v, %v; want type connected", hello, err)
	}
	return connection
}

// TestSupportSocketDeliversOnlyYourOwnEvents: an agent's reply reaches the
// customer's socket at once; another customer's socket stays quiet.
func TestSupportSocketDeliversOnlyYourOwnEvents(test *testing.T) {
	environment := testenv.Start(test)
	// the server program runs the hub; in tests we start it ourselves
	hubContext, stopHub := context.WithCancel(context.Background())
	test.Cleanup(stopHub)
	go environment.App.SupportHub.Run(hubContext)
	time.Sleep(200 * time.Millisecond) // let the Redis subscription start

	customer := environment.RegisterUser(test, "socketcustomer")
	stranger := environment.RegisterUser(test, "socketstranger")
	agent := environment.SignInAdmin(test, "support@tribe.africa")

	var thread struct {
		ID string `json:"id"`
	}
	customer.Call(test, http.MethodPost, "/api/mobile/v1/support/tickets", map[string]string{"subject": "Card help", "body": "My card was declined"}, testenv.NewKey()).Decode(test, &thread)
	if thread.ID == "" {
		test.Fatal("no ticket id")
	}

	mine := dialSupport(test, environment, customer)
	theirs := dialSupport(test, environment, stranger)

	reply := agent.Call(test, http.MethodPost, "/admin-api/support-threads/"+thread.ID+"/messages", map[string]string{"body": "Looking into it now"}, testenv.NewKey())
	if !reply.Success {
		test.Fatalf("agent reply: %d %s %s", reply.Status, reply.Code, reply.Message)
	}

	mine.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, payload, err := mine.ReadMessage()
	if err != nil {
		test.Fatalf("customer socket got nothing: %v", err)
	}
	var event struct {
		Type     string `json:"type"`
		ThreadID string `json:"thread_id"`
	}
	json.Unmarshal(payload, &event)
	if event.Type != "thread.updated" || event.ThreadID != thread.ID {
		test.Fatalf("customer socket event = %s, want thread.updated for %s", payload, thread.ID)
	}

	theirs.SetReadDeadline(time.Now().Add(800 * time.Millisecond))
	if _, leaked, err := theirs.ReadMessage(); err == nil {
		test.Fatalf("another customer's socket received %s", leaked)
	}

	// without a token the socket is refused (it sits behind the signed-in middleware)
	url := "ws" + strings.TrimPrefix(environment.Server.URL, "http") + "/api/mobile/v1/support/stream"
	if _, response, err := websocket.DefaultDialer.Dial(url, nil); err == nil || response == nil || response.StatusCode != http.StatusUnauthorized {
		status := 0
		if response != nil {
			status = response.StatusCode
		}
		test.Fatalf("anonymous dial gave %d, %v; want 401", status, err)
	}
}
