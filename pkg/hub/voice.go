package hub

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// Voice sessions (phase 2): the board sends its Gemini setup with
// session_start, mic audio up (channel 1) and tool answers; the hub holds the
// Gemini Live connection and sends the reply down as PCM (channel 2), paced.
// Resumption, goAway handoffs, the Google Search quota fallback, keeping the
// connection warm after a session and MCP tools are the hub's business.

const (
	DefaultGeminiURL = "wss://generativelanguage.googleapis.com/ws/google.ai.generativelanguage.v1beta.GenerativeService.BidiGenerateContent"

	uplinkFlagOpus = 0x02

	defaultVoiceLead  = 1500 * time.Millisecond
	geminiDialTimeout = 10 * time.Second
	geminiReadLimit   = 8 << 20
	micBufferMax      = 16000 * 2 * 4 // 4 s held while the connection is (re)made
	maxConnectTries   = 3             // per session, counting fallbacks and handoffs

	// The same line the board adds with Google Search in the setup
	// (GeminiProtocol::transmitSetupHandshake).
	searchInstruction = "You have Google Search. Use it for anything current or that you are unsure of: news, " +
		"prices, markets, sports, weather, events, recent facts. Never say you lack real-time " +
		"data without searching first."
)

// Board -> hub voice messages.
type sessionStartMsg struct {
	Setup      map[string]any `json:"setup"`
	WebSearch  bool           `json:"web_search"`
	KeepaliveS int            `json:"keepalive_s"`
	ResumeMin  int            `json:"resume_min"`
	Text       string         `json:"text"`
	// A new conversation (the board switched assistant profile): the setup
	// changed, so no kept connection and no resumption.
	Fresh bool `json:"fresh"`
}

type geminiConn struct {
	ws        *websocket.Conn
	cancel    context.CancelFunc
	ready     bool
	resuming  bool
	search    bool
	model     string
	replaced  bool // closed on purpose (handoff, new session): not an error
	connected time.Time
}

// voiceSession is the voice state of one board. It outlives the board's
// connection, so a reconnecting board can resume its conversation.
type voiceSession struct {
	h   *Hub
	id  string
	log *slog.Logger

	mu        sync.Mutex
	b         *edge // nil while the board is away
	active    bool  // a board session is running
	announced bool  // session_ready sent for this session
	tries     int
	gc        *geminiConn
	parkTimer *time.Timer

	start         sessionStartMsg
	boardTools    map[string]bool
	musicTools    bool // the hub's music tools replace the edge's
	handle        string
	handleAt      time.Time
	forceResume   bool
	searchRefused map[string]bool

	micBytes int // this session's mic audio, logged at its end

	opusDec      *opusDecoder
	uplinkFormat string
	opusAllowed  bool
	opusWarned   bool
	micPackets   int
	bytesIn      int
	decodeErrors int
	pcmBytes     int
	pcmScratch   []int16
	decScratch   []byte

	pendingMic  [][]byte
	pendingSize int
	pendingText string

	owed       bool // the person was heard / a tool answered: a reply is owed
	outputting bool // reply audio or a tool call since the last turn end
	dropReply  bool // the board interrupted: drop audio until the turn ends
	handoff    bool // goAway: move to a new connection at the next turn end
	endSent    bool // end_after_reply sent in this turn

	// Per reply, logged at its end.
	replyFirst, replyLast time.Time
	replyBytes            int
	replyMaxGap           time.Duration
}

func (h *Hub) voiceFor(id string) *voiceSession {
	h.mu.Lock()
	defer h.mu.Unlock()
	v := h.voices[id]
	if v == nil {
		v = &voiceSession{h: h, id: id, log: h.log.With("board", id), searchRefused: map[string]bool{}}
		h.voices[id] = v
	}
	return v
}

func (h *Hub) voiceEnabled() bool { return h.cfg.GeminiKey != "" }

func (v *voiceSession) attach(b *edge) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.b = b
	if b != nil {
		v.uplinkFormat = b.voiceUp
		v.opusAllowed = (b.voiceUp == "opus")
	}
}

// detach: the board's connection is gone; its session ends with it.
func (v *voiceSession) detach(b *edge) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.b != b {
		return
	}
	v.b = nil
	if v.active {
		v.log.Info("Board left during a voice session")
	}
	v.endLocked(false)
}

// ─────────────────────────────────────────────────────────────────────────────
// From the board
// ─────────────────────────────────────────────────────────────────────────────

func (v *voiceSession) fromBoard(typ string, data []byte) {
	switch typ {
	case "session_start":
		var m sessionStartMsg
		if err := json.Unmarshal(data, &m); err != nil || m.Setup == nil {
			v.sendBoard(map[string]any{"type": "session_error", "reason": "bad session_start"})
			return
		}
		v.startSession(m)
	case "text_turn":
		var m struct {
			Text string `json:"text"`
		}
		json.Unmarshal(data, &m)
		v.mu.Lock()
		if v.active && m.Text != "" {
			v.pendingText = m.Text
			v.owed = true
			v.flushPendingLocked()
		}
		v.mu.Unlock()
	case "tool_response":
		var m struct {
			ID     string          `json:"id"`
			Result json.RawMessage `json:"result"`
		}
		if json.Unmarshal(data, &m) != nil || m.ID == "" {
			return
		}
		v.mu.Lock()
		v.owed = true
		gc := v.gc
		v.mu.Unlock()
		v.sendToolResponse(gc, m.ID, m.Result)
	case "interrupt":
		v.mu.Lock()
		if v.b != nil {
			v.b.voiceQ.flush()
		}
		// Only a reply still arriving has audio left to drop.
		if v.outputting {
			v.dropReply = true
		}
		v.owed = true
		v.mu.Unlock()
	case "session_end":
		v.mu.Lock()
		v.endLocked(true)
		v.mu.Unlock()
	}
}

// mic handles a channel 1 binary frame payload.
// flags: byte 1 of channel 1 frame (bit 1: 0x02 = Opus, 0x00 = raw PCM).
// payload: byte 2.. of channel 1 frame.
func (v *voiceSession) mic(flags byte, payload []byte) {
	t0 := time.Now()
	v.mu.Lock()
	if w := time.Since(t0); w > 200*time.Millisecond {
		v.log.Warn("Mic waited for the session lock", "ms", w.Milliseconds())
	}
	defer v.mu.Unlock()
	if !v.active || len(payload) == 0 {
		return
	}
	v.bytesIn += len(payload)

	if flags&uplinkFlagOpus != 0 {
		if !v.opusAllowed && !v.opusWarned {
			v.opusWarned = true
			v.log.Warn("Opus frame received but opus was not negotiated for edge", "id", v.id)
		}
		if v.opusDec == nil {
			dec, err := newOpusDecoder(16000, 1)
			if err != nil {
				v.log.Error("Failed to create Opus decoder", "err", err)
				v.decodeErrors++
				return
			}
			v.opusDec = dec
		}

		if len(v.pcmScratch) < 1920 {
			v.pcmScratch = make([]int16, 1920)
		}
		v.decScratch = v.decScratch[:0]
		offset := 0
		for offset < len(payload) {
			if offset+2 > len(payload) {
				v.log.Warn("Malformed Opus frame: truncated packet length header", "offset", offset, "len", len(payload))
				v.decodeErrors++
				break
			}
			pktLen := int(binary.LittleEndian.Uint16(payload[offset : offset+2]))
			offset += 2
			if offset+pktLen > len(payload) {
				v.log.Warn("Malformed Opus frame: packet length exceeds frame", "pktLen", pktLen, "remaining", len(payload)-offset)
				v.decodeErrors++
				break
			}
			pkt := payload[offset : offset+pktLen]
			offset += pktLen

			n, err := v.opusDec.Decode(pkt, v.pcmScratch)
			if err != nil {
				v.log.Warn("Opus decode failed", "err", err)
				v.decodeErrors++
				continue
			}
			v.micPackets++
			for i := 0; i < n; i++ {
				s := uint16(v.pcmScratch[i])
				v.decScratch = append(v.decScratch, byte(s), byte(s>>8))
			}
		}

		if len(v.decScratch) > 0 {
			v.queueMicLocked(v.decScratch)
		}
	} else {
		v.micPackets++
		v.queueMicLocked(payload)
	}
}

func (v *voiceSession) queueMicLocked(pcm []byte) {
	v.pcmBytes += len(pcm)
	v.micBytes = v.pcmBytes
	if v.gc == nil || !v.gc.ready {
		buf := append([]byte(nil), pcm...)
		v.pendingMic = append(v.pendingMic, buf)
		v.pendingSize += len(buf)
		for v.pendingSize > micBufferMax && len(v.pendingMic) > 1 {
			v.pendingSize -= len(v.pendingMic[0])
			v.pendingMic = v.pendingMic[1:]
		}
		return
	}
	v.sendMicLocked(pcm)
}

func (v *voiceSession) startSession(m sessionStartMsg) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if !v.h.voiceEnabled() {
		v.sendBoardLocked(map[string]any{"type": "session_error", "reason": "the hub has no Gemini key"})
		return
	}
	if v.parkTimer != nil {
		v.parkTimer.Stop()
		v.parkTimer = nil
	}
	v.active = true
	v.endSent = false
	v.announced = false
	v.tries = 0
	v.start = m
	v.musicTools = v.h.music != nil && v.b != nil && !v.b.legacy
	v.boardTools = declaredNames(m.Setup)
	if v.musicTools {
		for n := range musicToolNames {
			delete(v.boardTools, n)
		}
		v.h.music.voiceActive(v.id, true)
	}
	if v.b != nil {
		v.uplinkFormat = v.b.voiceUp
		v.opusAllowed = (v.b.voiceUp == "opus")
	}
	if v.uplinkFormat == "" {
		v.uplinkFormat = "pcm"
	}
	if v.opusDec != nil {
		v.opusDec.Close()
		v.opusDec = nil
	}
	v.micPackets = 0
	v.bytesIn = 0
	v.decodeErrors = 0
	v.pcmBytes = 0
	v.opusWarned = false
	v.log.Info("Voice session starting", "uplink", v.uplinkFormat)

	v.pendingMic, v.pendingSize = nil, 0
	v.pendingText = m.Text
	v.owed = m.Text != ""
	v.outputting, v.dropReply = false, false
	if v.b != nil {
		v.b.voiceQ.flush()
	}
	if m.Fresh {
		v.handle = ""
		v.forceResume = false
	}
	if gc := v.gc; gc != nil && gc.ready && !v.handoff && !m.Fresh && gc.model == modelName(m.Setup) {
		v.log.Info("Voice session on the kept connection")
		v.announceLocked()
		v.flushPendingLocked()
		return
	}
	v.closeGeminiLocked()
	v.connectLocked()
}

// endLocked ends the board's session. keep: the connection may stay open
// for keepalive_s, if nothing is owed or arriving.
func (v *voiceSession) endLocked(keep bool) {
	wasActive := v.active
	v.active = false
	if wasActive {
		v.log.Info("Voice session ended",
			"mic_ms", v.pcmBytes/32,
			"packets", v.micPackets,
			"bytes_in", v.bytesIn,
			"decode_errors", v.decodeErrors,
		)
		if v.h.music != nil {
			v.h.music.voiceActive(v.id, false)
		}
	}
	if v.opusDec != nil {
		v.opusDec.Close()
		v.opusDec = nil
	}
	v.micBytes = 0
	v.micPackets = 0
	v.bytesIn = 0
	v.decodeErrors = 0
	v.pcmBytes = 0
	v.opusWarned = false
	v.pendingMic, v.pendingSize, v.pendingText = nil, 0, ""
	if v.b != nil {
		v.b.voiceQ.flush()
	}
	gc := v.gc
	if gc == nil {
		return
	}
	keepFor := time.Duration(v.start.KeepaliveS) * time.Second
	if keep && wasActive && gc.ready && keepFor > 0 && !v.owed && !v.outputting && !v.handoff {
		v.log.Info("Keeping the Gemini connection", "for", keepFor)
		v.parkTimer = time.AfterFunc(keepFor, func() {
			v.mu.Lock()
			defer v.mu.Unlock()
			if !v.active && v.gc == gc {
				v.log.Info("Keep-alive window over")
				v.closeGeminiLocked()
			}
		})
		return
	}
	v.closeGeminiLocked()
}

// ─────────────────────────────────────────────────────────────────────────────
// The Gemini connection
// ─────────────────────────────────────────────────────────────────────────────

func (v *voiceSession) connectLocked() {
	v.tries++
	setup, gc := v.buildSetupLocked()
	ctx, cancel := context.WithCancel(context.Background())
	gc.cancel = cancel
	v.gc = gc
	go v.runGemini(ctx, gc, setup)
}

// buildSetupLocked adds the hub's parts to the board's setup: Google Search
// (or the MCP search), the MCP tools and the resumption handle.
func (v *voiceSession) buildSetupLocked() ([]byte, *geminiConn) {
	setup := cloneJSON(v.start.Setup)
	model := modelName(setup)
	gc := &geminiConn{model: model}
	gc.search = v.start.WebSearch && strings.Contains(model, "2.5") && !v.searchRefused[model]

	tools, _ := setup["tools"].([]any)
	var first map[string]any
	if len(tools) > 0 {
		first, _ = tools[0].(map[string]any)
	}
	if first == nil {
		first = map[string]any{}
		tools = append([]any{first}, tools...)
	}
	decls, _ := first["functionDeclarations"].([]any)
	if v.musicTools {
		kept := decls[:0]
		for _, d := range decls {
			if dm, _ := d.(map[string]any); dm == nil || !musicToolNames[fmt.Sprint(dm["name"])] {
				kept = append(kept, d)
			}
		}
		decls = kept
		for _, d := range musicToolDeclarations() {
			decls = append(decls, d)
		}
	}
	if gc.search {
		// get_weather is the fallback for models without search.
		kept := decls[:0]
		for _, d := range decls {
			if dm, _ := d.(map[string]any); dm == nil || dm["name"] != "get_weather" {
				kept = append(kept, d)
			}
		}
		decls = kept
		tools = append(tools, map[string]any{"googleSearch": map[string]any{}})
		addInstruction(setup, searchInstruction)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	for _, d := range v.h.cfg.MCP.Declarations(ctx, v.boardTools, gc.search) {
		decls = append(decls, d)
	}
	cancel()
	first["functionDeclarations"] = decls
	setup["tools"] = tools

	resumption := map[string]any{}
	resumeFor := time.Duration(v.start.ResumeMin) * time.Minute
	if v.handle != "" && (v.forceResume || (resumeFor > 0 && time.Since(v.handleAt) < resumeFor)) {
		resumption["handle"] = v.handle
		gc.resuming = true
	}
	v.forceResume = false
	setup["sessionResumption"] = resumption

	data, _ := json.Marshal(map[string]any{"setup": setup})
	v.log.Info("Gemini setup", "model", model, "search", gc.search, "tools", len(decls),
		"resume", gc.resuming, "bytes", len(data))
	return data, gc
}

func (v *voiceSession) runGemini(ctx context.Context, gc *geminiConn, setup []byte) {
	dctx, cancel := context.WithTimeout(ctx, geminiDialTimeout)
	hdr := http.Header{}
	// The key goes in a header, not the URL: dial errors quote the URL.
	hdr.Set("x-goog-api-key", v.h.cfg.GeminiKey)
	ws, _, err := websocket.Dial(dctx, v.h.cfg.GeminiURL, &websocket.DialOptions{HTTPHeader: hdr})
	cancel()
	if err != nil {
		v.onGeminiClosed(gc, err)
		return
	}
	ws.SetReadLimit(geminiReadLimit)
	v.mu.Lock()
	gc.ws = ws
	gc.connected = time.Now()
	stale := v.gc != gc
	v.mu.Unlock()
	if stale {
		ws.Close(websocket.StatusNormalClosure, "")
		return
	}
	if err := ws.Write(ctx, websocket.MessageText, setup); err != nil {
		v.onGeminiClosed(gc, err)
		return
	}
	for {
		_, data, err := ws.Read(ctx)
		if err != nil {
			v.onGeminiClosed(gc, err)
			return
		}
		v.onGemini(gc, data)
	}
}

type geminiMsg struct {
	SetupComplete *json.RawMessage `json:"setupComplete"`
	ServerContent *struct {
		ModelTurn *struct {
			Parts []struct {
				InlineData *struct {
					Data string `json:"data"`
				} `json:"inlineData"`
			} `json:"parts"`
		} `json:"modelTurn"`
		TurnComplete        bool                   `json:"turnComplete"`
		Interrupted         bool                   `json:"interrupted"`
		InputTranscription  *struct{ Text string } `json:"inputTranscription"`
		OutputTranscription *struct{ Text string } `json:"outputTranscription"`
	} `json:"serverContent"`
	ToolCall *struct {
		FunctionCalls []struct {
			ID   string          `json:"id"`
			Name string          `json:"name"`
			Args json.RawMessage `json:"args"`
		} `json:"functionCalls"`
	} `json:"toolCall"`
	SessionResumptionUpdate *struct {
		NewHandle string `json:"newHandle"`
		Resumable bool   `json:"resumable"`
	} `json:"sessionResumptionUpdate"`
	GoAway *struct {
		TimeLeft string `json:"timeLeft"`
	} `json:"goAway"`
	Error *json.RawMessage `json:"error"`
}

func (v *voiceSession) onGemini(gc *geminiConn, data []byte) {
	var m geminiMsg
	if err := json.Unmarshal(data, &m); err != nil {
		v.log.Warn("Unreadable Gemini message", "bytes", len(data))
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.gc != gc {
		return
	}
	if m.SetupComplete != nil {
		gc.ready = true
		v.log.Info("Gemini setup complete", "resumed", gc.resuming,
			"ms", time.Since(gc.connected).Milliseconds())
		if v.active {
			v.announceLocked()
			v.flushPendingLocked()
		}
	}
	if u := m.SessionResumptionUpdate; u != nil && u.Resumable && u.NewHandle != "" {
		v.handle = u.NewHandle
		v.handleAt = time.Now()
	}
	if sc := m.ServerContent; sc != nil {
		if sc.InputTranscription != nil && sc.InputTranscription.Text != "" {
			v.owed = true
			v.sendBoardLocked(map[string]any{"type": "transcription", "role": "user", "text": sc.InputTranscription.Text})
		}
		if sc.ModelTurn != nil {
			for _, p := range sc.ModelTurn.Parts {
				if p.InlineData == nil || p.InlineData.Data == "" {
					continue
				}
				pcm, err := base64.StdEncoding.DecodeString(p.InlineData.Data)
				if err != nil {
					v.log.Warn("Bad audio in a Gemini message", "err", err)
					continue
				}
				v.noteAudioLocked(len(pcm))
				v.outputting = true
				v.owed = false
				if v.active && !v.dropReply && v.b != nil {
					v.b.voiceQ.pushPCM(pcm)
				}
			}
		}
		if sc.OutputTranscription != nil && sc.OutputTranscription.Text != "" && !v.dropReply {
			v.sendBoardLocked(map[string]any{"type": "transcription", "role": "model", "text": sc.OutputTranscription.Text})
		}
		if sc.Interrupted {
			v.log.Info("Reply interrupted (Gemini)")
			v.logReplyLocked("interrupted")
			v.dropReply = false
			if v.b != nil {
				v.b.voiceQ.flush()
				if v.active {
					v.b.voiceQ.pushText(mustJSON(map[string]any{"type": "interrupted"}))
				}
			}
		}
		if sc.TurnComplete {
			v.logReplyLocked("complete")
			v.endSent = false
			v.dropReply = false
			v.outputting = false
			v.owed = false
			if v.handle != "" {
				v.handleAt = time.Now()
			}
			if v.active && v.b != nil {
				v.b.voiceQ.pushText(mustJSON(map[string]any{"type": "turn_complete"}))
			}
			if v.handoff {
				v.handOffLocked()
			}
		}
	}
	if tc := m.ToolCall; tc != nil {
		v.outputting = true
		for _, fc := range tc.FunctionCalls {
			if fc.ID == "" {
				continue
			}
			if v.musicTools && musicToolNames[fc.Name] {
				v.sendBoardLocked(map[string]any{"type": "reply_owed"})
				go v.callMusic(gc, fc.ID, fc.Name, fc.Args)
				continue
			}
			if !v.boardTools[fc.Name] && v.h.cfg.MCP.Has(fc.Name) {
				v.sendBoardLocked(map[string]any{"type": "reply_owed"})
				go v.callMCP(gc, fc.ID, fc.Name, fc.Args)
				continue
			}
			args := fc.Args
			if len(args) == 0 {
				args = json.RawMessage("{}")
			}
			v.sendBoardLocked(map[string]any{"type": "tool_call", "id": fc.ID, "name": fc.Name, "args": args})
		}
	}
	if m.GoAway != nil {
		v.log.Info("Gemini goAway; handing off at the next turn end", "time_left", m.GoAway.TimeLeft)
		v.handoff = true
		if !v.owed && !v.outputting {
			v.handOffLocked()
		}
	}
	if m.Error != nil {
		v.log.Error("Gemini error message", "error", string(*m.Error))
		if v.active {
			v.sendBoardLocked(map[string]any{"type": "session_error", "reason": "gemini error"})
			v.active = false
		}
		v.closeGeminiLocked()
	}
}

func (v *voiceSession) onGeminiClosed(gc *geminiConn, err error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.gc != gc || gc.replaced {
		return
	}
	v.gc = nil
	gc.cancel()
	code := websocket.CloseStatus(err)
	reason := errText(err)
	v.log.Info("Gemini connection closed", "reason", reason, "ready", gc.ready)
	retry := false
	switch {
	case !gc.ready && gc.search && strings.Contains(reason, "quota"):
		v.log.Warn("Google Search refused (no quota); retrying without it", "model", gc.model)
		v.searchRefused[gc.model] = true
		retry = true
	case !gc.ready && gc.resuming && code != -1:
		v.log.Warn("Resumption refused; starting fresh")
		v.handle = ""
		retry = true
	case v.handoff:
		v.forceResume = true
		retry = true
	}
	if code == websocket.StatusInternalError {
		v.handle = ""
	}
	if v.handle != "" {
		v.handleAt = time.Now()
	}
	if retry && v.active && v.tries < maxConnectTries {
		v.handoff = false
		v.connectLocked()
		return
	}
	v.handoff = false
	if v.active {
		v.sendBoardLocked(map[string]any{"type": "session_error", "reason": reason})
		v.active = false
	}
}

// handOffLocked moves the session to a new connection resuming the same
// conversation (after goAway). Mic audio is held meanwhile.
func (v *voiceSession) handOffLocked() {
	v.handoff = false
	if v.handle == "" || !v.active {
		v.closeGeminiLocked()
		return
	}
	v.log.Info("Handing the session to a new Gemini connection")
	v.closeGeminiLocked()
	v.forceResume = true
	v.connectLocked()
}

func (v *voiceSession) closeGeminiLocked() {
	gc := v.gc
	v.gc = nil
	if v.parkTimer != nil {
		v.parkTimer.Stop()
		v.parkTimer = nil
	}
	if gc == nil {
		return
	}
	gc.replaced = true
	if v.handle != "" {
		v.handleAt = time.Now()
	}
	go func() {
		if gc.ws != nil {
			gc.ws.Close(websocket.StatusNormalClosure, "")
		}
		gc.cancel()
	}()
}

// ─────────────────────────────────────────────────────────────────────────────
// Sending
// ─────────────────────────────────────────────────────────────────────────────

func (v *voiceSession) announceLocked() {
	if v.announced {
		return
	}
	v.announced = true
	v.sendBoardLocked(map[string]any{"type": "session_ready", "resumed": v.gc != nil && v.gc.resuming})
}

func (v *voiceSession) flushPendingLocked() {
	if v.gc == nil || !v.gc.ready {
		return
	}
	for _, pcm := range v.pendingMic {
		v.sendMicLocked(pcm)
	}
	v.pendingMic, v.pendingSize = nil, 0
	if v.pendingText != "" {
		v.writeGeminiLocked(map[string]any{"realtimeInput": map[string]any{"text": v.pendingText}})
		v.pendingText = ""
	}
}

func (v *voiceSession) sendMicLocked(pcm []byte) {
	v.writeGeminiLocked(map[string]any{"realtimeInput": map[string]any{"audio": map[string]any{
		"mimeType": "audio/pcm;rate=16000", "data": base64.StdEncoding.EncodeToString(pcm)}}})
}

func (v *voiceSession) writeGeminiLocked(msg any) {
	gc := v.gc
	if gc == nil || gc.ws == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
	defer cancel()
	t0 := time.Now()
	if err := gc.ws.Write(ctx, websocket.MessageText, mustJSON(msg)); err != nil {
		v.log.Warn("Write to Gemini failed", "err", err)
	}
	if w := time.Since(t0); w > 200*time.Millisecond {
		v.log.Warn("Slow write to Gemini", "ms", w.Milliseconds())
	}
}

func (v *voiceSession) sendToolResponse(gc *geminiConn, id string, result json.RawMessage) {
	if len(result) == 0 || !json.Valid(result) {
		result = json.RawMessage("{}")
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if gc == nil || v.gc != gc {
		v.log.Warn("Tool response for a closed connection", "id", id)
		return
	}
	v.writeGeminiLocked(map[string]any{"toolResponse": map[string]any{"functionResponses": []any{
		map[string]any{"id": id, "response": map[string]any{"output": result}}}}})
}

func (v *voiceSession) callMCP(gc *geminiConn, id, name string, args json.RawMessage) {
	ctx, cancel := context.WithTimeout(context.Background(), mcpCallTimeout)
	defer cancel()
	t0 := time.Now()
	res := v.h.cfg.MCP.Call(ctx, name, args)
	v.log.Info("MCP tool", "name", name, "ms", time.Since(t0).Milliseconds(), "bytes", len(res))
	v.sendToolResponse(gc, id, res)
}

func (v *voiceSession) callMusic(gc *geminiConn, id, name string, args json.RawMessage) {
	v.mu.Lock()
	from := v.b
	v.mu.Unlock()
	t0 := time.Now()
	res := v.h.music.toolCall(from, name, args)
	v.log.Info("Music tool", "name", name, "ms", time.Since(t0).Milliseconds(), "result", string(res))
	var st struct {
		Status string `json:"status"`
	}
	if json.Unmarshal(res, &st) == nil && st.Status == "success" {
		// As in direct mode, a music command ends the session once the reply
		// has played (the music may be on another speaker the mic can hear).
		v.mu.Lock()
		if v.active && !v.endSent {
			v.endSent = true
			v.sendBoardLocked(map[string]any{"type": "end_after_reply"})
		}
		v.mu.Unlock()
	}
	v.sendToolResponse(gc, id, res)
}

func (v *voiceSession) sendBoard(msg any) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.sendBoardLocked(msg)
}

func (v *voiceSession) sendBoardLocked(msg any) {
	if v.b == nil {
		return
	}
	select {
	case v.b.send <- mustJSON(msg):
	default:
		v.log.Warn("Board send queue full; dropped a voice message")
	}
}

// ─────────────────────────────────────────────────────────────────────────────

func (v *voiceSession) noteAudioLocked(n int) {
	now := time.Now()
	if v.replyFirst.IsZero() {
		v.replyFirst = now
	} else if gap := now.Sub(v.replyLast); gap > v.replyMaxGap {
		v.replyMaxGap = gap
	}
	v.replyLast = now
	v.replyBytes += n
}

// logReplyLocked: how fast Gemini delivered the reply to the hub.
func (v *voiceSession) logReplyLocked(how string) {
	if v.replyBytes == 0 {
		return
	}
	audio := time.Duration(v.replyBytes/2) * time.Second / voiceRate
	span := v.replyLast.Sub(v.replyFirst)
	rate := 0.0
	if span > 0 {
		rate = float64(audio) / float64(span)
	}
	v.log.Info("Reply delivery", "how", how, "audio_ms", audio.Milliseconds(), "span_ms", span.Milliseconds(),
		"rate", fmt.Sprintf("%.2fx", rate), "max_gap_ms", v.replyMaxGap.Milliseconds())
	v.replyFirst, v.replyLast, v.replyBytes, v.replyMaxGap = time.Time{}, time.Time{}, 0, 0
}

func modelName(setup map[string]any) string {
	s, _ := setup["model"].(string)
	return s
}

func declaredNames(setup map[string]any) map[string]bool {
	names := map[string]bool{}
	tools, _ := setup["tools"].([]any)
	for _, t := range tools {
		tm, _ := t.(map[string]any)
		decls, _ := tm["functionDeclarations"].([]any)
		for _, d := range decls {
			if dm, _ := d.(map[string]any); dm != nil {
				if n, _ := dm["name"].(string); n != "" {
					names[n] = true
				}
			}
		}
	}
	return names
}

func addInstruction(setup map[string]any, text string) {
	si, _ := setup["systemInstruction"].(map[string]any)
	if si == nil {
		si = map[string]any{}
		setup["systemInstruction"] = si
	}
	parts, _ := si["parts"].([]any)
	if len(parts) > 0 {
		if p, _ := parts[0].(map[string]any); p != nil {
			if t, _ := p["text"].(string); t != "" {
				p["text"] = t + "\n\n" + text
				return
			}
		}
	}
	si["parts"] = append([]any{map[string]any{"text": text}}, parts...)
}

func cloneJSON(m map[string]any) map[string]any {
	data, _ := json.Marshal(m)
	var out map[string]any
	json.Unmarshal(data, &out)
	return out
}

func mustJSON(v any) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		panic(errors.Join(errors.New("hub: unencodable message"), err))
	}
	return data
}
