package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func announceReq(t *testing.T, h http.Handler, token, from, body string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, AnnouncePath, strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:5555"
	req.Header.Set("X-Real-IP", from)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

func listFrom(t *testing.T, h http.Handler, from string) []DirectoryHub {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, DirectoryPath+"?format=json", nil)
	req.RemoteAddr = "127.0.0.1:5555"
	req.Header.Set("X-Real-IP", from)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out struct{ Hubs []DirectoryHub }
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("list: %v (%s)", err, rec.Body.String())
	}
	return out.Hubs
}

func TestDirectoryAnnounceAndMatch(t *testing.T) {
	h := New(Config{Tokens: []string{testToken}, Directory: true, DirectoryAll: true})
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	h.dir.now = func() time.Time { return now }
	srv := h.Handler()

	// Bad token, bad body: refused.
	if c := announceReq(t, srv, "nope", "203.0.113.5", `{"name":"hp","lan_url":"http://192.168.1.5:8380/"}`); c != 401 {
		t.Fatalf("bad token: %d", c)
	}
	if c := announceReq(t, srv, testToken, "203.0.113.5", `{"name":"hp","lan_url":"https://evil.example/"}`); c != 400 {
		t.Fatalf("public lan_url: %d", c)
	}
	if c := announceReq(t, srv, testToken, "203.0.113.5", `{"name":"<b>","lan_url":"http://192.168.1.5:8380/"}`); c != 400 {
		t.Fatalf("bad name: %d", c)
	}

	// hp announces over IPv4 and IPv6; pi from another network.
	for _, a := range []struct{ from, body string }{
		{"203.0.113.5", `{"name":"hp","lan_url":"http://192.168.1.5:8380/"}`},
		{"2001:db8:1:2::abcd", `{"name":"hp","lan_url":"http://192.168.1.5:8380/"}`},
		{"198.51.100.7", `{"name":"pi","lan_url":"http://192.168.0.9:8380/"}`},
	} {
		if c := announceReq(t, srv, testToken, a.from, a.body); c != 200 {
			t.Fatalf("announce %s: %d", a.from, c)
		}
	}

	// Same public IPv4, and same IPv6 /64 (another device): hp is here.
	for _, viewer := range []string{"203.0.113.5", "2001:db8:1:2::1"} {
		hubs := listFrom(t, srv, viewer)
		if len(hubs) != 2 || hubs[0].Name != "hp" || !hubs[0].Here || hubs[1].Here || !hubs[1].Online {
			t.Fatalf("viewer %s: %+v", viewer, hubs)
		}
	}
	// Another /64, another IPv4: nothing here.
	for _, viewer := range []string{"2001:db8:1:3::1", "192.0.2.1"} {
		for _, hb := range listFrom(t, srv, viewer) {
			if hb.Here {
				t.Fatalf("viewer %s: %s marked here", viewer, hb.Name)
			}
		}
	}

	// Silent for too long: offline, not here; then forgotten.
	now = now.Add(directoryOnline + time.Second)
	hubs := listFrom(t, srv, "203.0.113.5")
	if len(hubs) != 2 || hubs[0].Online || hubs[0].Here {
		t.Fatalf("offline: %+v", hubs)
	}
	now = now.Add(directoryForget)
	if hubs := listFrom(t, srv, "203.0.113.5"); len(hubs) != 0 {
		t.Fatalf("forgotten: %+v", hubs)
	}
}

func TestDirectoryShowsOnlyHereWithoutAll(t *testing.T) {
	srv := New(Config{Tokens: []string{testToken}, Directory: true}).Handler()
	announceReq(t, srv, testToken, "203.0.113.5", `{"name":"hp","lan_url":"http://192.168.1.5:8380/"}`)
	announceReq(t, srv, testToken, "198.51.100.7", `{"name":"pi","lan_url":"http://192.168.0.9:8380/"}`)
	if hubs := listFrom(t, srv, "203.0.113.5"); len(hubs) != 1 || hubs[0].Name != "hp" || !hubs[0].Here {
		t.Fatalf("home viewer: %+v", hubs)
	}
	if hubs := listFrom(t, srv, "192.0.2.1"); len(hubs) != 0 {
		t.Fatalf("stranger sees: %+v", hubs)
	}
}

func TestDirectoryOffByDefault(t *testing.T) {
	srv := New(Config{Tokens: []string{testToken}}).Handler()
	if c := announceReq(t, srv, testToken, "203.0.113.5", `{"name":"hp","lan_url":"http://192.168.1.5:8380/"}`); c == 200 {
		t.Fatal("announce accepted without Directory")
	}
}

func TestClientAddrTrustsOnlyLocalProxy(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "198.51.100.1:443"
	req.Header.Set("X-Real-IP", "203.0.113.5")
	if got := clientAddr(req); got != netip.MustParseAddr("198.51.100.1") {
		t.Fatalf("remote peer: X-Real-IP trusted (%s)", got)
	}
	req.RemoteAddr = "[::1]:443"
	if got := clientAddr(req); got != netip.MustParseAddr("203.0.113.5") {
		t.Fatalf("local proxy: %s", got)
	}
}

func TestDirectoryPageEscapes(t *testing.T) {
	h := New(Config{Tokens: []string{testToken}, Directory: true})
	srv := h.Handler()
	announceReq(t, srv, testToken, "203.0.113.5", `{"name":"hp","lan_url":"http://192.168.1.5:8380/?a=<x>"}`)
	req := httptest.NewRequest(http.MethodGet, DirectoryPath, nil)
	req.RemoteAddr = "127.0.0.1:1"
	req.Header.Set("X-Real-IP", "203.0.113.5")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "On this network") || !strings.Contains(body, "192.168.1.5:8380") || strings.Contains(body, "<x>") {
		t.Fatalf("page: %s", body)
	}
}

func TestAnnouncerPosts(t *testing.T) {
	dirHub := New(Config{Tokens: []string{testToken}, Directory: true})
	ts := httptest.NewServer(dirHub.Handler())
	defer ts.Close()
	ctx, cancel := context.WithCancel(context.Background())
	a := &Announcer{URL: ts.URL, Token: func() string { return testToken }, Name: "hp", Version: "t",
		LANURL: func() string { return "http://192.168.1.5:8380/" }, Interval: time.Hour}
	done := make(chan struct{})
	go func() { a.Run(ctx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		dirHub.dir.mu.Lock()
		e := dirHub.dir.hubs["hp"]
		dirHub.dir.mu.Unlock()
		if e != nil {
			if e.lanURL != "http://192.168.1.5:8380/" {
				t.Fatalf("lan url %q", e.lanURL)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no announce arrived")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
}
