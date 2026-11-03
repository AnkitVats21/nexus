package hub

import (
	"encoding/json"
	"html/template"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// The directory: a list of the hubs at home, for a hub on a public server
// (--directory). Each home hub announces itself every minute
// (Announcer); the page at DirectoryPath lists them, the ones on the
// viewer's network first, with a link to each hub's LAN address. A hub is
// on the viewer's network when it announced from the same public IPv4
// address, or from the same IPv6 /64.
//
// Without a login in front of it (the default), the page shows only the
// hubs on the viewer's own network: a name and a private address, of use
// only inside that network. DirectoryAll also lists the others; put a login
// in front of it then. Announces need a hub token (Config.Tokens).

const (
	DirectoryPath = "/hub/directory"
	AnnouncePath  = "/hub/directory/announce"

	// A hub not heard from for this long is shown as offline.
	directoryOnline = 3 * time.Minute
	// ... and dropped after this.
	directoryForget = 7 * 24 * time.Hour
	// The address a hub announced from counts for "same network" this long.
	directoryAddrTTL = 5 * time.Minute
)

type announceMsg struct {
	Name    string `json:"name"`
	LANURL  string `json:"lan_url"`
	Version string `json:"version"`
}

type dirEntry struct {
	name    string
	lanURL  string
	version string
	seen    time.Time
	v4      netip.Addr // the public addresses it announced from
	v4Seen  time.Time
	v6      netip.Addr
	v6Seen  time.Time
}

type directory struct {
	tokenOK func(string) bool
	all     bool // list the hubs on other networks too
	now     func() time.Time

	mu   sync.Mutex
	hubs map[string]*dirEntry // by name
}

func newDirectory(tokenOK func(string) bool, all bool) *directory {
	return &directory{tokenOK: tokenOK, all: all, now: time.Now, hubs: map[string]*dirEntry{}}
}

// clientAddr is the address a request came from: the proxy's X-Real-IP when
// the request came through a proxy on this machine, else the peer.
func clientAddr(r *http.Request) netip.Addr {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}
	}
	peer = peer.Unmap()
	if peer.IsLoopback() {
		if real, err := netip.ParseAddr(strings.TrimSpace(r.Header.Get("X-Real-IP"))); err == nil {
			return real.Unmap()
		}
	}
	return peer
}

// validLANURL: http(s) to a private, link-local or loopback IP address (no
// names: the link must not lead anywhere else).
func validLANURL(s string) bool {
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
		return false
	}
	ip, err := netip.ParseAddr(strings.Trim(u.Hostname(), "[]"))
	if err != nil {
		return false
	}
	return ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLoopback()
}

func validHubName(s string) bool {
	if s == "" || len(s) > 40 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}

func (d *directory) serveAnnounce(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || !d.tokenOK(strings.TrimSpace(token)) {
		http.Error(w, "bad token", http.StatusUnauthorized)
		return
	}
	var msg announceMsg
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&msg); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if !validHubName(msg.Name) || !validLANURL(msg.LANURL) || len(msg.Version) > 32 {
		http.Error(w, "bad name or lan_url", http.StatusBadRequest)
		return
	}
	from := clientAddr(r)
	now := d.now()
	d.mu.Lock()
	e := d.hubs[msg.Name]
	if e == nil {
		e = &dirEntry{name: msg.Name}
		d.hubs[msg.Name] = e
	}
	e.lanURL, e.version, e.seen = msg.LANURL, msg.Version, now
	switch {
	case from.Is4():
		e.v4, e.v4Seen = from, now
	case from.Is6():
		e.v6, e.v6Seen = from, now
	}
	d.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"ok":true}`))
}

// DirectoryHub is one row of the list.
type DirectoryHub struct {
	Name     string `json:"name"`
	URL      string `json:"url"`
	Version  string `json:"version,omitempty"`
	Online   bool   `json:"online"`
	Here     bool   `json:"here"` // on the viewer's network
	LastSeen string `json:"last_seen"`
}

func sameNetwork(a, b netip.Addr) bool {
	if !a.IsValid() || !b.IsValid() || a.Is4() != b.Is4() {
		return false
	}
	if a.Is4() {
		return a == b
	}
	pa, _ := a.Prefix(64)
	pb, _ := b.Prefix(64)
	return pa == pb
}

// list: the hubs for a viewer at `viewer`: here first, then online, then
// by name.
func (d *directory) list(viewer netip.Addr) []DirectoryHub {
	now := d.now()
	d.mu.Lock()
	out := make([]DirectoryHub, 0, len(d.hubs))
	for name, e := range d.hubs {
		age := now.Sub(e.seen)
		if age > directoryForget {
			delete(d.hubs, name)
			continue
		}
		online := age <= directoryOnline
		here := online && ((now.Sub(e.v4Seen) <= directoryAddrTTL && sameNetwork(viewer, e.v4)) ||
			(now.Sub(e.v6Seen) <= directoryAddrTTL && sameNetwork(viewer, e.v6)))
		if !here && !d.all {
			continue
		}
		out = append(out, DirectoryHub{Name: e.name, URL: e.lanURL, Version: e.version, Online: online,
			Here: here, LastSeen: e.seen.UTC().Format(time.RFC3339)})
	}
	d.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Here != b.Here {
			return a.Here
		}
		if a.Online != b.Online {
			return a.Online
		}
		return a.Name < b.Name
	})
	return out
}

func (d *directory) servePage(w http.ResponseWriter, r *http.Request) {
	hubs := d.list(clientAddr(r))
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.Query().Get("format") == "json" || strings.Contains(r.Header.Get("Accept"), "application/json") {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"hubs": hubs})
		return
	}
	var here, elsewhere []DirectoryHub
	for _, h := range hubs {
		if h.Here {
			here = append(here, h)
		} else {
			elsewhere = append(elsewhere, h)
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	directoryPage.Execute(w, map[string]any{"Here": here, "Elsewhere": elsewhere})
}

var directoryPage = template.Must(template.New("dir").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Nexus hubs</title>
<style>
:root { --bg:#f6f7f9; --card:#fff; --fg:#1b1f24; --muted:#667085; --accent:#2563eb; --line:#e4e7ec; --ok:#16a34a; }
@media (prefers-color-scheme: dark) { :root { --bg:#0f1115; --card:#181b21; --fg:#e6e8eb; --muted:#9aa3af; --accent:#60a5fa; --line:#2a2f37; --ok:#4ade80; } }
body { margin:0; background:var(--bg); color:var(--fg); font:16px/1.45 system-ui, sans-serif; }
main { max-width:560px; margin:0 auto; padding:24px 16px; }
h1 { font-size:22px; margin:0 0 20px; }
h2 { font-size:13px; text-transform:uppercase; letter-spacing:.05em; color:var(--muted); margin:24px 0 8px; }
a.hub, div.hub { display:flex; align-items:center; gap:12px; padding:14px 16px; background:var(--card);
  border:1px solid var(--line); border-radius:12px; margin-bottom:8px; color:inherit; text-decoration:none; }
a.hub:hover { border-color:var(--accent); }
.dot { width:10px; height:10px; border-radius:50%; background:var(--muted); flex:none; }
.on .dot { background:var(--ok); }
.name { font-weight:600; }
.sub { color:var(--muted); font-size:13px; }
.go { margin-left:auto; color:var(--accent); font-weight:600; }
p.empty { color:var(--muted); }
</style></head><body><main>
<h1>Nexus hubs</h1>
<h2>On this network</h2>
{{range .Here}}<a class="hub on" href="{{.URL}}"><span class="dot"></span><span><div class="name">{{.Name}}</div><div class="sub">{{.URL}}</div></span><span class="go">Open</span></a>
{{else}}<p class="empty">No hub found on this network.</p>{{end}}
{{if .Elsewhere}}<h2>Elsewhere</h2>
{{range .Elsewhere}}<div class="hub{{if .Online}} on{{end}}"><span class="dot"></span><span><div class="name">{{.Name}}</div><div class="sub">{{if .Online}}online, {{.URL}} on its own network{{else}}offline, last seen <time datetime="{{.LastSeen}}">{{.LastSeen}}</time>{{end}}</div></span></div>
{{end}}{{end}}
</main>
<script>
for (const t of document.querySelectorAll("time")) {
  const d = new Date(t.dateTime); if (!isNaN(d)) t.textContent = d.toLocaleString();
}
</script>
</body></html>
`))
