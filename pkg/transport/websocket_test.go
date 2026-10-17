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

func TestWebSocketTransportTLS(t *testing.T) {
	var receivedLock sync.Mutex
	var receivedMsgs []string

	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")

		_, data, err := conn.Read(r.Context())
		if err == nil {
			receivedLock.Lock()
			receivedMsgs = append(receivedMsgs, string(data))
			receivedLock.Unlock()

			_ = conn.Write(r.Context(), websocket.MessageText, []byte("echo:"+string(data)))
		}
	}))
	defer s.Close()

	wssURL := "wss" + strings.TrimPrefix(s.URL, "https")

	// 1. Connection without trusting the self-signed test cert should fail TLS verification
	untrustedTransport := NewWebSocketTransport()
	defer untrustedTransport.Close()

	untrustedConnected := make(chan struct{})
	untrustedTransport.OnConnected(func() {
		close(untrustedConnected)
	})
	_ = untrustedTransport.Connect(context.Background(), wssURL)

	select {
	case <-untrustedConnected:
		t.Fatal("expected connection to fail TLS verification with untrusted cert")
	case <-time.After(500 * time.Millisecond):
		// Expected: TLS handshake verification failed
	}

	// 2. Connection with test cert pool injected via test hook should succeed with full verification
	trustedTransport := NewWebSocketTransport()
	defer trustedTransport.Close()

	trustedTransport.SetRootCAs(s.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs)

	connectedCh := make(chan struct{})
	trustedTransport.OnConnected(func() {
		close(connectedCh)
	})

	if err := trustedTransport.Connect(context.Background(), wssURL); err != nil {
		t.Fatalf("failed to connect over wss: %v", err)
	}

	select {
	case <-connectedCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for OnConnected over wss")
	}

	if err := trustedTransport.Send([]byte("hello-tls")); err != nil {
		t.Fatalf("failed to send: %v", err)
	}

	msgCh, _ := trustedTransport.Receive()
	select {
	case msg := <-msgCh:
		if string(msg) != "echo:hello-tls" {
			t.Errorf("expected 'echo:hello-tls', got '%s'", string(msg))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for echo reply over wss")
	}
}
