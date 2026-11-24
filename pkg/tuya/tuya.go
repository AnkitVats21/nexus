// Package tuya talks to a Tuya Wi-Fi device over its local protocol, version
// 3.5 (the 0x6699 frame: AES-GCM with a per-connection session key). It is
// enough for a bulb: read the data points (DPs) and set some.
//
// A Conn is one TCP connection to the device (port 6668) with its session key
// negotiated. Devices take few connections at once: open one, use it, close it.
package tuya

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"
)

// Commands of the local protocol.
const (
	CmdSessKeyStart  = 3
	CmdSessKeyResp   = 4
	CmdSessKeyFinish = 5
	CmdStatus        = 8  // the device reports its DPs
	CmdControlNew    = 13 // set DPs
	CmdQueryNew      = 16 // ask for the DPs
)

const (
	prefix    = 0x00006699
	suffix    = 0x00009966
	headerLen = 18 // prefix(4) unknown(2) seq(4) cmd(4) length(4)
	ivLen     = 12
	tagLen    = 16
	maxFrame  = 64 << 10

	// Port is the device's local TCP port.
	Port = 6668
)

// versionHeader goes before the payload of commands that carry one (control).
var versionHeader = append([]byte("3.5"), make([]byte, 12)...)

// Device is how to reach one device.
type Device struct {
	ID  string // device id (22 characters)
	IP  string // its LAN address; Port is added
	Key string // local key (16 characters)
	// Timeout bounds dialing and each exchange; 0 = 5 s.
	Timeout time.Duration
}

// Conn is a connection with its session key.
type Conn struct {
	c       net.Conn
	key     []byte // session key
	seq     uint32
	timeout time.Duration
}

// Dial connects and negotiates the session key.
func (d Device) Dial(ctx context.Context) (*Conn, error) {
	if len(d.Key) != 16 {
		return nil, errors.New("tuya: local key must be 16 characters")
	}
	timeout := d.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	dialer := net.Dialer{Timeout: timeout}
	nc, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(d.IP, strconv.Itoa(Port)))
	if err != nil {
		return nil, fmt.Errorf("tuya: %w", err)
	}
	c := &Conn{c: nc, key: []byte(d.Key), seq: 1, timeout: timeout}
	if dl, ok := ctx.Deadline(); ok {
		_ = nc.SetDeadline(dl)
	} else {
		_ = nc.SetDeadline(time.Now().Add(timeout))
	}
	if err := c.negotiate(); err != nil {
		nc.Close()
		return nil, err
	}
	return c, nil
}

// Close closes the connection.
func (c *Conn) Close() error { return c.c.Close() }

func (c *Conn) negotiate() error {
	localKey := c.key
	local := make([]byte, 16)
	if _, err := rand.Read(local); err != nil {
		return err
	}
	if err := c.send(CmdSessKeyStart, local); err != nil {
		return err
	}
	cmd, p, err := c.recv()
	if err != nil {
		return err
	}
	if cmd != CmdSessKeyResp || len(p) < 48 {
		return fmt.Errorf("tuya: session key: unexpected reply (cmd %d, %d bytes)", cmd, len(p))
	}
	remote := p[:16]
	if !hmac.Equal(p[16:48], mac(localKey, local)) {
		return errors.New("tuya: session key: device failed the check (wrong local key?)")
	}
	if err := c.send(CmdSessKeyFinish, mac(localKey, remote)); err != nil {
		return err
	}
	c.key = SessionKey(localKey, local, remote)
	return nil
}

// SessionKey derives the 3.5 session key from the local key and both nonces.
func SessionKey(localKey, local, remote []byte) []byte {
	x := make([]byte, 16)
	for i := range x {
		x[i] = local[i] ^ remote[i]
	}
	g := newGCM(localKey)
	return g.Seal(nil, local[:ivLen], x, nil)[:16]
}

func mac(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

func newGCM(key []byte) cipher.AEAD {
	b, err := aes.NewCipher(key)
	if err != nil {
		panic(err) // the key length is checked before
	}
	g, err := cipher.NewGCM(b)
	if err != nil {
		panic(err)
	}
	return g
}

// Status returns the device's DPs.
func (c *Conn) Status() (map[string]any, error) {
	if err := c.send(CmdQueryNew, []byte("{}")); err != nil {
		return nil, err
	}
	return c.waitDPs()
}

// Set sets DPs and returns the DPs the device reports back (often only the
// changed ones).
func (c *Conn) Set(dps map[string]any) (map[string]any, error) {
	body, err := json.Marshal(map[string]any{
		"protocol": 5,
		"t":        time.Now().Unix(),
		"data":     map[string]any{"dps": dps},
	})
	if err != nil {
		return nil, err
	}
	if err := c.send(CmdControlNew, append(append([]byte{}, versionHeader...), body...)); err != nil {
		return nil, err
	}
	// The device acks, then reports the DPs that changed: none changed, no
	// report. After the ack, a short wait for it is enough.
	_ = c.c.SetReadDeadline(time.Now().Add(c.timeout))
	acked := false
	for {
		cmd, p, err := c.recv()
		if err != nil {
			var ne net.Error
			if acked && errors.As(err, &ne) && ne.Timeout() {
				return map[string]any{}, nil
			}
			return nil, err
		}
		if dps, ok := ParseDPs(p); ok {
			return dps, nil
		}
		if cmd == CmdControlNew && !acked {
			acked = true
			_ = c.c.SetReadDeadline(time.Now().Add(time.Second))
		}
	}
}

// waitDPs reads frames until one carries DPs. The device acknowledges a
// command with an empty frame before it reports.
func (c *Conn) waitDPs() (map[string]any, error) {
	_ = c.c.SetReadDeadline(time.Now().Add(c.timeout))
	for {
		_, p, err := c.recv()
		if err != nil {
			return nil, err
		}
		if dps, ok := ParseDPs(p); ok {
			return dps, nil
		}
	}
}

// ParseDPs finds the DPs in a payload: {"dps":{...}} or {"data":{"dps":{...}}},
// after the version header if there is one ("3.5" and 12 bytes, which the
// device fills: not always zeros).
func ParseDPs(p []byte) (map[string]any, bool) {
	if bytes.HasPrefix(p, []byte("3.")) && len(p) >= len(versionHeader) {
		p = p[len(versionHeader):]
	}
	if len(p) == 0 || p[0] != '{' {
		return nil, false
	}
	var m struct {
		DPs  map[string]any `json:"dps"`
		Data struct {
			DPs map[string]any `json:"dps"`
		} `json:"data"`
	}
	if json.Unmarshal(p, &m) != nil {
		return nil, false
	}
	if m.DPs != nil {
		return m.DPs, true
	}
	if m.Data.DPs != nil {
		return m.Data.DPs, true
	}
	return nil, false
}

func (c *Conn) send(cmd uint32, payload []byte) error {
	f, err := EncodeFrame(c.key, c.seq, cmd, payload)
	if err != nil {
		return err
	}
	c.seq++
	if _, err := c.c.Write(f); err != nil {
		return fmt.Errorf("tuya: send: %w", err)
	}
	return nil
}

// recv reads one frame and returns its command and payload, the return code
// (the first 4 bytes of what a device sends) taken off.
func (c *Conn) recv() (uint32, []byte, error) {
	cmd, p, err := ReadFrame(c.c, c.key)
	if err != nil {
		return 0, nil, err
	}
	if len(p) >= 4 {
		p = p[4:]
	}
	return cmd, p, nil
}

// EncodeFrame builds a 0x6699 frame.
func EncodeFrame(key []byte, seq, cmd uint32, payload []byte) ([]byte, error) {
	iv := make([]byte, ivLen)
	if _, err := rand.Read(iv); err != nil {
		return nil, err
	}
	h := make([]byte, headerLen)
	binary.BigEndian.PutUint32(h[0:], prefix)
	binary.BigEndian.PutUint32(h[6:], seq)
	binary.BigEndian.PutUint32(h[10:], cmd)
	binary.BigEndian.PutUint32(h[14:], uint32(ivLen+len(payload)+tagLen))
	out := append(h, iv...)
	out = newGCM(key).Seal(out, iv, payload, h[4:headerLen])
	return binary.BigEndian.AppendUint32(out, suffix), nil
}

// ReadFrame reads and decrypts one 0x6699 frame.
func ReadFrame(r io.Reader, key []byte) (cmd uint32, payload []byte, err error) {
	h := make([]byte, headerLen)
	if _, err := io.ReadFull(r, h); err != nil {
		return 0, nil, fmt.Errorf("tuya: read: %w", err)
	}
	if binary.BigEndian.Uint32(h) != prefix {
		return 0, nil, errors.New("tuya: not a 3.5 frame")
	}
	cmd = binary.BigEndian.Uint32(h[10:])
	n := binary.BigEndian.Uint32(h[14:])
	if n < ivLen+tagLen || n > maxFrame {
		return 0, nil, fmt.Errorf("tuya: bad frame length %d", n)
	}
	body := make([]byte, n+4)
	if _, err := io.ReadFull(r, body); err != nil {
		return 0, nil, fmt.Errorf("tuya: read: %w", err)
	}
	if binary.BigEndian.Uint32(body[n:]) != suffix {
		return 0, nil, errors.New("tuya: bad frame suffix")
	}
	payload, err = newGCM(key).Open(nil, body[:ivLen], body[ivLen:n], h[4:headerLen])
	if err != nil {
		return 0, nil, errors.New("tuya: frame does not decrypt (wrong key?)")
	}
	return cmd, payload, nil
}
