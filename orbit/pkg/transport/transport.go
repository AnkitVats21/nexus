package transport

import "context"

// Transport defines the communication channel between Satellite and Nexus Core.
type Transport interface {
	Connect(ctx context.Context, endpoint string) error
	Send(msg []byte) error
	Receive() (<-chan []byte, <-chan error)
	OnConnected(fn func())
	Close() error
}
