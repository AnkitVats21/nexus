package transport

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestWebSocketTransport(t *testing.T) {
	var receivedLock sync.Mutex
	var receivedMsgs []string

	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")

		// Read one message and echo it back
		_, data, err := conn.Read(r.Context())
		if err == nil {
			receivedLock.Lock()
			receivedMsgs = append(receivedMsgs, string(data))
			receivedLock.Unlock()

			_ = conn.Write(r.Context(), websocket.MessageText, []byte("echo:"+string(data)))
		}
	}))
	defer s.Close()

	wsURL := "ws" + strings.TrimPrefix(s.URL, "http")

	transport := NewWebSocketTransport()
	defer transport.Close()

	connectedCh := make(chan struct{})
	transport.OnConnected(func() {
		close(connectedCh)
	})

	if err := transport.Connect(context.Background(), wsURL); err != nil {
		t.Fatalf("failed to connect: %v", err)
	}

	select {
	case <-connectedCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for OnConnected")
	}

	if err := transport.Send([]byte("hello-nexus")); err != nil {
		t.Fatalf("failed to send: %v", err)
	}

	msgCh, _ := transport.Receive()
	select {
	case msg := <-msgCh:
		if string(msg) != "echo:hello-nexus" {
			t.Errorf("expected 'echo:hello-nexus', got '%s'", string(msg))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for echo reply")
	}
}
