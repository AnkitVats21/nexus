package hub

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"math"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func newVoiceHubWithUplink(t *testing.T, g *fakeGemini, uplink string) *httptest.Server {
	h := New(Config{
		Version:   "test",
		Tokens:    []string{testToken},
		GeminiKey: "gemini-key",
		GeminiURL: g.url(),
		Uplink:    uplink,
		VoiceLead: 100 * time.Millisecond,
	})
	srv := httptest.NewServer(h.Handler())
	t.Cleanup(srv.Close)
	return srv
}

func TestNegotiationTable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	g := newFakeGemini(t)

	tests := []struct {
		name       string
		hubUplink  string
		helloRoles *edgeRoles
		legacy     bool
		wantUp     string
	}{
		{
			name:      "legacy proto 1 hello defaults to pcm",
			hubUplink: "auto",
			legacy:    true,
			wantUp:    "pcm",
		},
		{
			name:       "nil voice roles defaults to pcm",
			hubUplink:  "auto",
			helloRoles: &edgeRoles{},
			wantUp:     "pcm",
		},
		{
			name:       "empty up list defaults to pcm",
			hubUplink:  "auto",
			helloRoles: &edgeRoles{Voice: &struct {
				Up   []string `json:"up"`
				Down []string `json:"down"`
			}{Up: nil, Down: []string{"pcm24"}}},
			wantUp: "pcm",
		},
		{
			name:       "pcm16 only edge gets pcm under auto",
			hubUplink:  "auto",
			helloRoles: &edgeRoles{Voice: &struct {
				Up   []string `json:"up"`
				Down []string `json:"down"`
			}{Up: []string{"pcm16"}, Down: []string{"pcm24"}}},
			wantUp: "pcm",
		},
		{
			name:       "opus16 offered gets opus under auto",
			hubUplink:  "auto",
			helloRoles: &edgeRoles{Voice: &struct {
				Up   []string `json:"up"`
				Down []string `json:"down"`
			}{Up: []string{"opus16", "pcm16"}, Down: []string{"pcm24"}}},
			wantUp: "opus",
		},
		{
			name:       "opus16 offered gets opus with hub uplink=opus",
			hubUplink:  "opus",
			helloRoles: &edgeRoles{Voice: &struct {
				Up   []string `json:"up"`
				Down []string `json:"down"`
			}{Up: []string{"opus16"}, Down: []string{"pcm24"}}},
			wantUp: "opus",
		},
		{
			name:       "pcm-only edge gets pcm even with hub uplink=opus",
			hubUplink:  "opus",
			helloRoles: &edgeRoles{Voice: &struct {
				Up   []string `json:"up"`
				Down []string `json:"down"`
			}{Up: []string{"pcm16"}, Down: []string{"pcm24"}}},
			wantUp: "pcm",
		},
		{
			name:       "opus-offering edge gets pcm when hub uplink=pcm",
			hubUplink:  "pcm",
			helloRoles: &edgeRoles{Voice: &struct {
				Up   []string `json:"up"`
				Down []string `json:"down"`
			}{Up: []string{"opus16", "pcm16"}, Down: []string{"pcm24"}}},
			wantUp: "pcm",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := newVoiceHubWithUplink(t, g, tc.hubUplink)
			wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + EdgePath

			conn := dial(t, ctx, wsURL)
			defer conn.Close(websocket.StatusNormalClosure, "done")

			hello := map[string]any{
				"type":  "hello",
				"proto": 2,
				"id":    "test-edge-" + tc.name,
				"name":  "Edge",
				"token": testToken,
			}
			if !tc.legacy && tc.helloRoles != nil {
				hello["roles"] = tc.helloRoles
			}
			write(t, ctx, conn, hello)

			welcome := read(t, ctx, conn)
			if welcome["type"] != "welcome" {
				t.Fatalf("expected welcome, got %v", welcome)
			}
			v, ok := welcome["voice"].(map[string]any)
			if !ok {
				t.Fatalf("welcome voice is not a map: %v", welcome["voice"])
			}
			if gotUp := v["up"]; gotUp != tc.wantUp {
				t.Fatalf("voice.up = %v, want %v", gotUp, tc.wantUp)
			}
		})
	}
}

func generateSinePCM16(sampleRate int, freq float64, count int) []int16 {
	pcm := make([]int16, count)
	for i := range pcm {
		pcm[i] = int16(16000 * math.Sin(2*math.Pi*freq*float64(i)/float64(sampleRate)))
	}
	return pcm
}

func TestOpusRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	g := newFakeGemini(t)
	srv := newVoiceHubWithUplink(t, g, "auto")

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + EdgePath
	conn := dial(t, ctx, wsURL)
	defer conn.Close(websocket.StatusNormalClosure, "done")

	roles := &edgeRoles{Voice: &struct {
		Up   []string `json:"up"`
		Down []string `json:"down"`
	}{Up: []string{"opus16", "pcm16"}, Down: []string{"pcm24"}}}

	write(t, ctx, conn, map[string]any{
		"type":  "hello",
		"proto": 2,
		"id":    "test-opus-rt",
		"token": testToken,
		"roles": roles,
	})
	welcome := read(t, ctx, conn)
	vInfo, _ := welcome["voice"].(map[string]any)
	if vInfo["up"] != "opus" {
		t.Fatalf("expected welcome voice.up opus, got %v", vInfo["up"])
	}

	br := &boardReader{t: t, conn: conn}

	// Start voice session
	write(t, ctx, conn, map[string]any{
		"type":  "session_start",
		"setup": boardSetup(),
	})
	br.next(ctx, "session_ready")

	// Encode 1 kHz sine at 16 kHz with RESTRICTED_LOWDELAY into 20 ms packets
	// 20 ms at 16 kHz = 320 samples
	const sampleRate = 16000
	const frameSamples = 320
	const numPackets = 10
	sinePCM := generateSinePCM16(sampleRate, 1000.0, frameSamples*numPackets)

	enc, err := newOpusEncoder(sampleRate, 1, opusAppRestrictedLowdelay)
	if err != nil {
		t.Fatalf("newOpusEncoder failed: %v", err)
	}
	defer enc.Close()

	var opusPackets [][]byte
	outBuf := make([]byte, 1000)
	for i := 0; i < numPackets; i++ {
		chunk := sinePCM[i*frameSamples : (i+1)*frameSamples]
		n, err := enc.Encode(chunk, frameSamples, outBuf)
		if err != nil {
			t.Fatalf("Opus encode packet %d failed: %v", i, err)
		}
		pkt := make([]byte, n)
		copy(pkt, outBuf[:n])
		opusPackets = append(opusPackets, pkt)
	}

	// Pack several per frame (e.g. 2 packets per frame)
	// Framing: byte 0: chanMic (1), byte 1: flags (0x02 for Opus), then [len(2B LE), packet]...
	for i := 0; i < len(opusPackets); i += 2 {
		frame := []byte{chanMic, uplinkFlagOpus}
		for j := 0; j < 2 && i+j < len(opusPackets); j++ {
			pkt := opusPackets[i+j]
			var lenBytes [2]byte
			binary.LittleEndian.PutUint16(lenBytes[:], uint16(len(pkt)))
			frame = append(frame, lenBytes[:]...)
			frame = append(frame, pkt...)
		}
		if err := conn.Write(ctx, websocket.MessageBinary, frame); err != nil {
			t.Fatalf("Write opus frame failed: %v", err)
		}
	}

	// Allow time for processing
	time.Sleep(150 * time.Millisecond)

	// End session
	write(t, ctx, conn, map[string]any{"type": "session_end"})
	time.Sleep(100 * time.Millisecond)

	// Verify PCM audio reached fakeGemini
	_, _, inputs := g.snapshot()
	var totalReceivedPCM []byte
	for _, in := range inputs {
		if ri, ok := in["realtimeInput"].(map[string]any); ok {
			if audio, ok := ri["audio"].(map[string]any); ok {
				if dataStr, ok := audio["data"].(string); ok {
					decoded, err := base64.StdEncoding.DecodeString(dataStr)
					if err == nil {
						totalReceivedPCM = append(totalReceivedPCM, decoded...)
					}
				}
			}
		}
	}

	// Expected PCM bytes: 10 packets * 320 samples * 2 bytes = 6400 bytes
	expectedBytes := numPackets * frameSamples * 2
	oneFrameBytes := frameSamples * 2
	diff := int(math.Abs(float64(len(totalReceivedPCM) - expectedBytes)))
	if diff > oneFrameBytes {
		t.Fatalf("total PCM bytes received = %d, expected %d (allowed diff <= %d)", len(totalReceivedPCM), expectedBytes, oneFrameBytes)
	}

	// Check non-trivial energy
	sampleCount := len(totalReceivedPCM) / 2
	if sampleCount == 0 {
		t.Fatalf("no PCM samples received")
	}
	var sumSq float64
	for i := 0; i < sampleCount; i++ {
		s := int16(binary.LittleEndian.Uint16(totalReceivedPCM[i*2 : (i+1)*2]))
		sumSq += float64(s) * float64(s)
	}
	rms := math.Sqrt(sumSq / float64(sampleCount))
	if rms < 3000.0 {
		t.Fatalf("received audio energy too low (rms=%f, expected > 3000.0 for 1kHz sine)", rms)
	}
}

func TestMalformedOpusFrames(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	g := newFakeGemini(t)
	srv := newVoiceHubWithUplink(t, g, "auto")

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + EdgePath
	conn := dial(t, ctx, wsURL)
	defer conn.Close(websocket.StatusNormalClosure, "done")

	roles := &edgeRoles{Voice: &struct {
		Up   []string `json:"up"`
		Down []string `json:"down"`
	}{Up: []string{"opus16"}, Down: []string{"pcm24"}}}

	write(t, ctx, conn, map[string]any{
		"type":  "hello",
		"proto": 2,
		"id":    "test-malformed-edge",
		"token": testToken,
		"roles": roles,
	})
	read(t, ctx, conn)

	br := &boardReader{t: t, conn: conn}
	write(t, ctx, conn, map[string]any{
		"type":  "session_start",
		"setup": boardSetup(),
	})
	br.next(ctx, "session_ready")

	// 1. Truncated length header
	conn.Write(ctx, websocket.MessageBinary, []byte{chanMic, uplinkFlagOpus, 0x05})

	// 2. Length extends past frame
	conn.Write(ctx, websocket.MessageBinary, []byte{chanMic, uplinkFlagOpus, 0x10, 0x00, 0x01, 0x02})

	// 3. Corrupted packet
	conn.Write(ctx, websocket.MessageBinary, []byte{chanMic, uplinkFlagOpus, 0x04, 0x00, 0xff, 0xff, 0xff, 0xff})

	// Hub should drop them without killing the session.
	// Verify session is alive by sending a valid text turn and getting a reply.
	write(t, ctx, conn, map[string]any{"type": "text_turn", "text": "hello"})
	br.next(ctx, "turn_complete")

	write(t, ctx, conn, map[string]any{"type": "session_end"})
}

func TestMixedOpusPCMFormatSession(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	g := newFakeGemini(t)
	srv := newVoiceHubWithUplink(t, g, "auto")

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + EdgePath
	conn := dial(t, ctx, wsURL)
	defer conn.Close(websocket.StatusNormalClosure, "done")

	roles := &edgeRoles{Voice: &struct {
		Up   []string `json:"up"`
		Down []string `json:"down"`
	}{Up: []string{"opus16", "pcm16"}, Down: []string{"pcm24"}}}

	write(t, ctx, conn, map[string]any{
		"type":  "hello",
		"proto": 2,
		"id":    "test-mixed-edge",
		"token": testToken,
		"roles": roles,
	})
	read(t, ctx, conn)

	br := &boardReader{t: t, conn: conn}
	write(t, ctx, conn, map[string]any{
		"type":  "session_start",
		"setup": boardSetup(),
	})
	br.next(ctx, "session_ready")

	enc, err := newOpusEncoder(16000, 1, opusAppRestrictedLowdelay)
	if err != nil {
		t.Fatalf("newOpusEncoder failed: %v", err)
	}
	defer enc.Close()

	sinePCM := generateSinePCM16(16000, 1000.0, 320*4)
	outBuf := make([]byte, 1000)

	encodePacket := func(chunk []int16) []byte {
		n, err := enc.Encode(chunk, 320, outBuf)
		if err != nil {
			t.Fatalf("encode failed: %v", err)
		}
		pkt := make([]byte, n)
		copy(pkt, outBuf[:n])
		return pkt
	}

	pkt1 := encodePacket(sinePCM[0:320])
	pkt2 := encodePacket(sinePCM[320:640])
	pkt3 := encodePacket(sinePCM[640:960])
	pkt4 := encodePacket(sinePCM[960:1280])

	// 1. Opus frame with 2 packets (320 samples each = 640 samples = 1280 bytes PCM)
	var f1 []byte
	f1 = append(f1, chanMic, uplinkFlagOpus)
	var l1 [2]byte
	binary.LittleEndian.PutUint16(l1[:], uint16(len(pkt1)))
	f1 = append(f1, l1[:]...)
	f1 = append(f1, pkt1...)
	var l2 [2]byte
	binary.LittleEndian.PutUint16(l2[:], uint16(len(pkt2)))
	f1 = append(f1, l2[:]...)
	f1 = append(f1, pkt2...)
	conn.Write(ctx, websocket.MessageBinary, f1)

	// 2. PCM frame with 640 samples (1280 bytes PCM), flags bit 1 is 0 (0x00)
	rawPCMBytes := make([]byte, 640*2)
	for i := 0; i < 640; i++ {
		s := sinePCM[i]
		rawPCMBytes[i*2] = byte(s)
		rawPCMBytes[i*2+1] = byte(s >> 8)
	}
	f2 := append([]byte{chanMic, 0x00}, rawPCMBytes...)
	conn.Write(ctx, websocket.MessageBinary, f2)

	// 3. Opus frame again with 2 packets (640 samples = 1280 bytes PCM)
	var f3 []byte
	f3 = append(f3, chanMic, uplinkFlagOpus)
	var l3 [2]byte
	binary.LittleEndian.PutUint16(l3[:], uint16(len(pkt3)))
	f3 = append(f3, l3[:]...)
	f3 = append(f3, pkt3...)
	var l4 [2]byte
	binary.LittleEndian.PutUint16(l4[:], uint16(len(pkt4)))
	f3 = append(f3, l4[:]...)
	f3 = append(f3, pkt4...)
	conn.Write(ctx, websocket.MessageBinary, f3)

	time.Sleep(150 * time.Millisecond)
	write(t, ctx, conn, map[string]any{"type": "session_end"})
	time.Sleep(100 * time.Millisecond)

	_, _, inputs := g.snapshot()
	var totalReceivedPCM []byte
	for _, in := range inputs {
		if ri, ok := in["realtimeInput"].(map[string]any); ok {
			if audio, ok := ri["audio"].(map[string]any); ok {
				if dataStr, ok := audio["data"].(string); ok {
					decoded, err := base64.StdEncoding.DecodeString(dataStr)
					if err == nil {
						totalReceivedPCM = append(totalReceivedPCM, decoded...)
					}
				}
			}
		}
	}

	// Expected: 1280 + 1280 + 1280 = 3840 bytes PCM
	expectedTotal := 1280 * 3
	diff := int(math.Abs(float64(len(totalReceivedPCM) - expectedTotal)))
	if diff > 640 {
		t.Fatalf("mixed session received %d PCM bytes, expected %d (diff <= 640)", len(totalReceivedPCM), expectedTotal)
	}
}
