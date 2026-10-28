package hub

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestBluetoothProxy(t *testing.T) {
	var gotPath, gotBody string
	orbit := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotPath, gotBody = r.Method+" "+r.URL.Path, string(b)
		w.Write([]byte(`{"status":"ok"}`))
	}))
	api, _ := url.Parse(orbit.URL)
	srv := httptest.NewServer(New(Config{BluetoothAPI: api}).Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/hub/bluetooth")
	if err != nil || resp.StatusCode != 200 || gotPath != "GET /bluetooth" {
		t.Fatalf("get: %v %v %q", err, resp, gotPath)
	}
	resp, _ = http.Post(srv.URL+"/hub/bluetooth/pair", "application/json", strings.NewReader(`{"mac":"x"}`))
	if resp.StatusCode != 200 || gotPath != "POST /bluetooth/pair" || gotBody != `{"mac":"x"}` {
		t.Fatalf("post: %d %q %q", resp.StatusCode, gotPath, gotBody)
	}

	var st map[string]any
	resp, _ = http.Get(srv.URL + "/hub/status")
	json.NewDecoder(resp.Body).Decode(&st)
	if st["bluetooth"] != true {
		t.Fatalf("status %v", st)
	}

	orbit.Close()
	resp, _ = http.Get(srv.URL + "/hub/bluetooth")
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("orbit down: %d", resp.StatusCode)
	}

	// Without the flag there is no route (the dashboard's index.html).
	plain := httptest.NewServer(New(Config{}).Handler())
	defer plain.Close()
	resp, _ = http.Get(plain.URL + "/hub/status")
	st = nil
	json.NewDecoder(resp.Body).Decode(&st)
	if st["bluetooth"] != false {
		t.Fatalf("status without: %v", st)
	}
}
