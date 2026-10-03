package transport

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// WebSocketTransport implements Transport over WebSocket.
type WebSocketTransport struct {
	endpoint string
	log      *slog.Logger

	connMu sync.Mutex
	conn   *websocket.Conn

	msgCh chan []byte
	errCh chan error

	onConnected   func()
	onConnectedMu sync.RWMutex

	closed atomic.Bool
	ctx    context.Context
	cancel context.CancelFunc
}

// NewWebSocketTransport creates a new WebSocketTransport.
func NewWebSocketTransport() *WebSocketTransport {
	ctx, cancel := context.WithCancel(context.Background())
	return &WebSocketTransport{
		log:    slog.Default().With("component", "transport_ws"),
		msgCh:  make(chan []byte, 64),
		errCh:  make(chan error, 16),
		ctx:    ctx,
		cancel: cancel,
	}
}

func (t *WebSocketTransport) OnConnected(fn func()) {
	t.onConnectedMu.Lock()
	defer t.onConnectedMu.Unlock()
	t.onConnected = fn
}

func (t *WebSocketTransport) Connect(ctx context.Context, endpoint string) error {
	t.endpoint = endpoint
	go t.reconnectLoop()
	return nil
}

func (t *WebSocketTransport) Send(msg []byte) error {
	t.connMu.Lock()
	conn := t.conn
	t.connMu.Unlock()

	if conn == nil {
		return errors.New("websocket not connected")
	}

	writeCtx, cancel := context.WithTimeout(t.ctx, 3*time.Second)
	defer cancel()

	return conn.Write(writeCtx, websocket.MessageText, msg)
}

func (t *WebSocketTransport) Receive() (<-chan []byte, <-chan error) {
	return t.msgCh, t.errCh
}

func (t *WebSocketTransport) Close() error {
	if t.closed.Swap(true) {
		return nil
	}

	t.cancel()

	t.connMu.Lock()
	defer t.connMu.Unlock()
	if t.conn != nil {
		_ = t.conn.Close(websocket.StatusNormalClosure, "client closing")
	}
	return nil
}

func (t *WebSocketTransport) reconnectLoop() {
	backoff := 1 * time.Second
	maxBackoff := 30 * time.Second

	for !t.closed.Load() {
		t.log.Info("Connecting to Nexus Core WebSocket", "endpoint", t.endpoint)

		dialCtx, dialCancel := context.WithTimeout(t.ctx, 5*time.Second)
		conn, _, err := websocket.Dial(dialCtx, t.endpoint, nil)
		dialCancel()

		if err != nil {
			t.log.Warn("Failed to connect to Nexus Core", "err", err, "retryIn", backoff)
			select {
			case <-t.ctx.Done():
				return
			case <-time.After(backoff):
				// Exponential backoff with jitter
				jitter := time.Duration(rand.Int63n(int64(500 * time.Millisecond)))
				backoff = backoff*2 + jitter
				if backoff > maxBackoff {
					backoff = maxBackoff
				}
				continue
			}
		}

		t.log.Info("Connected to Nexus Core", "endpoint", t.endpoint)
		backoff = 1 * time.Second

		t.connMu.Lock()
		t.conn = conn
		t.connMu.Unlock()

		// Trigger onConnected callback
		t.onConnectedMu.RLock()
		onConn := t.onConnected
		t.onConnectedMu.RUnlock()
		if onConn != nil {
			go onConn()
		}

		// Read until disconnect
		t.readLoop(conn)

		t.connMu.Lock()
		t.conn = nil
		t.connMu.Unlock()

		if !t.closed.Load() {
			t.log.Warn("Connection dropped, reconnecting...")
		}
	}
}

func (t *WebSocketTransport) readLoop(conn *websocket.Conn) {
	for !t.closed.Load() {
		msgType, data, err := conn.Read(t.ctx)
		if err != nil {
			if !t.closed.Load() {
				select {
				case t.errCh <- fmt.Errorf("read error: %w", err):
				default:
				}
			}
			return
		}

		if msgType == websocket.MessageText {
			select {
			case t.msgCh <- data:
			default:
				t.log.Warn("Message channel full, dropping frame")
			}
		}
	}
}
