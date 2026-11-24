package tuya

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"net"
	"testing"
	"time"
)

const testKey = "0123456789abcdef"

// fakeDevice speaks the device side of the protocol on one connection: it
// negotiates the session key, answers a query with dps and applies a control.
func fakeDevice(t *testing.T, nc net.Conn, dps map[string]any) {
	defer nc.Close()
	key := []byte(testKey)
	reply := func(k []byte, cmd uint32, p []byte) {
		f, err := EncodeFrame(k, 1, cmd, append([]byte{0, 0, 0, 0}, p...))
		if err != nil {
			t.Error(err)
			return
		}
		nc.Write(f)
	}
	cmd, local, err := ReadFrame(nc, key)
	if err != nil || cmd != CmdSessKeyStart {
		t.Errorf("start: cmd %d err %v", cmd, err)
		return
	}
	remote := make([]byte, 16)
	rand.Read(remote)
	reply(key, CmdSessKeyResp, append(append([]byte{}, remote...), mac(key, local)...))
	cmd, fin, err := ReadFrame(nc, key)
	if err != nil || cmd != CmdSessKeyFinish || !bytes.Equal(fin, mac(key, remote)) {
		t.Errorf("finish: cmd %d err %v", cmd, err)
		return
	}
	sk := SessionKey(key, local, remote)
	for {
		cmd, p, err := ReadFrame(nc, sk)
		if err != nil {
			return
		}
		switch cmd {
		case CmdQueryNew:
			b, _ := json.Marshal(map[string]any{"dps": dps})
			reply(sk, CmdQueryNew, b)
		case CmdControlNew:
			var m struct {
				Data struct {
					DPs map[string]any `json:"dps"`
				} `json:"data"`
			}
			if !bytes.HasPrefix(p, versionHeader) || json.Unmarshal(p[len(versionHeader):], &m) != nil {
				t.Errorf("bad control payload %q", p)
				return
			}
			reply(sk, CmdControlNew, nil) // the ack, no DPs
			b, _ := json.Marshal(map[string]any{"protocol": 4, "data": map[string]any{"dps": m.Data.DPs}})
			reply(sk, CmdStatus, append(append([]byte{}, versionHeader...), b...))
		}
	}
}

func TestStatusAndSet(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		nc, err := ln.Accept()
		if err == nil {
			fakeDevice(t, nc, map[string]any{"20": true, "21": "white"})
		}
	}()

	c := &Conn{key: []byte(testKey), seq: 1, timeout: 2 * time.Second}
	c.c, err = (&net.Dialer{}).DialContext(context.Background(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.negotiate(); err != nil {
		t.Fatal(err)
	}
	dps, err := c.Status()
	if err != nil || dps["20"] != true || dps["21"] != "white" {
		t.Fatalf("status = %v, %v", dps, err)
	}
	got, err := c.Set(map[string]any{"20": false})
	if err != nil || got["20"] != false {
		t.Fatalf("set = %v, %v", got, err)
	}
}

func TestWrongKey(t *testing.T) {
	f, _ := EncodeFrame([]byte(testKey), 1, CmdStatus, []byte("x"))
	if _, _, err := ReadFrame(bytes.NewReader(f), []byte("fedcba9876543210")); err == nil {
		t.Fatal("a frame decrypted with the wrong key")
	}
}

func TestParseDPs(t *testing.T) {
	for _, p := range []string{`{"dps":{"20":true}}`, `{"protocol":4,"data":{"dps":{"20":true}}}`,
		// as the bulb sends it: the version header's 12 bytes not zeros
		"3.5\x00\x00\x00\x00\x00\x00Mu\x00\x00\x00\x01" + `{"protocol":4,"t":1,"data":{"dps":{"20":true}}}`} {
		if dps, ok := ParseDPs([]byte(p)); !ok || dps["20"] != true {
			t.Errorf("%s: %v %v", p, dps, ok)
		}
	}
	if _, ok := ParseDPs(nil); ok {
		t.Error("empty payload has DPs")
	}
}
