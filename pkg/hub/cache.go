package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// MusicCache keeps the songs the hub has looked up (WebM/Opus, as YouTube
// sends them) and serves them to the players over the LAN at
// /hub/cache/<id>.webm. A player then gets the song from the hub instead of
// YouTube: no TLS, no CDN throttle, and no lookup for a song played before.
// The least recently played go first once the folder passes its size.

const (
	CachePath           = "/hub/cache/"
	cacheDownloadTimout = 10 * time.Minute
	cacheMinBytes       = 32 << 10 // smaller: a broken download (the board's rule too)
	// Longer songs (mixes, podcasts) would fill the cache; an unknown length
	// counts as long, as on the board (NexusPlayer MAX_CACHE_DURATION_MS).
	cacheMaxDuration = 20 * time.Minute
)

var cacheID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`) // YouTube ids are 11; no path characters

type MusicCache struct {
	dir  string
	max  int64
	http *http.Client
	log  *slog.Logger

	mu      sync.Mutex
	pending map[string]bool
}

// NewMusicCache keeps up to maxBytes in dir (created if missing).
func NewMusicCache(dir string, maxBytes int64, log *slog.Logger) (*MusicCache, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.Default()
	}
	// Leftovers of downloads cut short.
	if tmps, _ := filepath.Glob(filepath.Join(dir, "*.tmp")); len(tmps) > 0 {
		for _, t := range tmps {
			os.Remove(t)
		}
	}
	return &MusicCache{dir: dir, max: maxBytes, http: &http.Client{Timeout: cacheDownloadTimout},
		log: log.With("component", "cache"), pending: map[string]bool{}}, nil
}

func (c *MusicCache) path(id string) string { return filepath.Join(c.dir, id+".webm") }

// Has reports whether the song is saved, and marks it used.
func (c *MusicCache) Has(id string) bool {
	if c == nil || !cacheID.MatchString(id) {
		return false
	}
	p := c.path(id)
	if st, err := os.Stat(p); err != nil || st.Size() < cacheMinBytes {
		return false
	}
	now := time.Now()
	os.Chtimes(p, now, now)
	return true
}

// URL is where an edge that reached the hub at base ("http://host:port")
// gets the song.
func (c *MusicCache) URL(base, id string) string {
	return base + CachePath + id + ".webm"
}

// Fetch saves the song (durationS long) from its stream URL in the
// background, unless it is saved or on its way, or its length is unknown or
// over cacheMaxDuration.
func (c *MusicCache) Fetch(id, streamURL string, durationS int) {
	if c == nil || !cacheID.MatchString(id) || streamURL == "" || c.Has(id) {
		return
	}
	if d := time.Duration(durationS) * time.Second; d <= 0 || d > cacheMaxDuration {
		c.log.Info("Not caching: length unknown or over the limit", "id", id, "s", durationS, "max_s", int(cacheMaxDuration.Seconds()))
		return
	}
	c.mu.Lock()
	if c.pending[id] {
		c.mu.Unlock()
		return
	}
	c.pending[id] = true
	c.mu.Unlock()
	go func() {
		defer func() {
			c.mu.Lock()
			delete(c.pending, id)
			c.mu.Unlock()
		}()
		t0 := time.Now()
		n, err := c.download(id, streamURL)
		if err != nil {
			c.log.Warn("Cache download failed", "id", id, "err", err)
			return
		}
		c.log.Info("Cached", "id", id, "kb", n>>10, "ms", time.Since(t0).Milliseconds())
		c.evict()
	}()
}

func (c *MusicCache) download(id, streamURL string) (int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), cacheDownloadTimout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, streamURL, nil)
	if err != nil {
		return 0, err
	}
	// YouTube's CDN throttles to about real time without a Range header.
	req.Header.Set("Range", "bytes=0-")
	resp, err := c.http.Do(req)
	if err != nil {
		// The error text carries the URL (signed, IP-bound): keep the host.
		var ue *url.Error
		if ok := asURLError(err, &ue); ok {
			return 0, fmt.Errorf("%s: %v", hostOf(streamURL), ue.Err)
		}
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return 0, fmt.Errorf("%s: HTTP %d", hostOf(streamURL), resp.StatusCode)
	}
	tmp := c.path(id) + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(f, resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n < cacheMinBytes {
		err = fmt.Errorf("only %d bytes", n)
	}
	if err == nil && resp.ContentLength > 0 && n != resp.ContentLength {
		err = fmt.Errorf("cut short at %d of %d bytes", n, resp.ContentLength)
	}
	if err != nil {
		os.Remove(tmp)
		return 0, err
	}
	return n, os.Rename(tmp, c.path(id))
}

// evict removes the least recently used songs until the folder fits.
func (c *MusicCache) evict() {
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return
	}
	type file struct {
		path string
		size int64
		used time.Time
	}
	var files []file
	var total int64
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".webm") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, file{filepath.Join(c.dir, e.Name()), info.Size(), info.ModTime()})
		total += info.Size()
	}
	if total <= c.max {
		return
	}
	sort.Slice(files, func(i, j int) bool { return files[i].used.Before(files[j].used) })
	for _, f := range files {
		if total <= c.max {
			break
		}
		if os.Remove(f.path) == nil {
			total -= f.size
			c.log.Info("Evicted", "file", filepath.Base(f.path), "kb", f.size>>10)
		}
	}
}

// CacheList is what GET /hub/cache answers: the songs kept (id: bytes) and
// the space used and allowed.
type CacheList struct {
	Enabled   bool             `json:"enabled"`
	MaxBytes  int64            `json:"max_bytes"`
	UsedBytes int64            `json:"used_bytes"`
	MaxDurS   int              `json:"max_duration_s"`
	Songs     map[string]int64 `json:"songs"`
}

// List returns the songs kept (a nil cache: not enabled).
func (c *MusicCache) List() CacheList {
	l := CacheList{Songs: map[string]int64{}}
	if c == nil {
		return l
	}
	l.Enabled, l.MaxBytes, l.MaxDurS = true, c.max, int(cacheMaxDuration.Seconds())
	entries, _ := os.ReadDir(c.dir)
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".webm")
		if !ok || !cacheID.MatchString(id) {
			continue
		}
		info, err := e.Info()
		if err != nil || info.Size() < cacheMinBytes {
			continue
		}
		l.Songs[id] = info.Size()
		l.UsedBytes += info.Size()
	}
	return l
}

// ServeList serves GET /hub/cache: List as JSON.
func (c *MusicCache) ServeList(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(c.List())
}

// ServeHTTP serves /hub/cache/<id>.webm (Range requests too: seeking).
func (c *MusicCache) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, CachePath)
	id := strings.TrimSuffix(name, ".webm")
	if id == name || !c.Has(id) {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "audio/webm")
	http.ServeFile(w, r, c.path(id))
}

func hostOf(raw string) string {
	if u, err := url.Parse(raw); err == nil {
		return u.Host
	}
	return "?"
}

func asURLError(err error, target **url.Error) bool {
	ue, ok := err.(*url.Error)
	if ok {
		*target = ue
	}
	return ok
}
