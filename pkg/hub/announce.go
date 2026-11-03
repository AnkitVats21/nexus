package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"
)

// Announcer tells a directory (a hub with --directory, see directory.go)
// that this hub is online and where it is on the LAN. It announces every
// Interval, once over IPv4 and once over IPv6, so the directory knows both
// public addresses of this network: a viewer's browser may come over either.
type Announcer struct {
	URL      string        // the directory's base URL, e.g. https://hub.example.com
	Token    func() string // a hub token the directory accepts
	Name     string
	LANURL   func() string // this hub's address on the LAN ("" = skip this round)
	Version  string
	Interval time.Duration // default 60 s
	Log      *slog.Logger
}

// Run announces until ctx ends.
func (a *Announcer) Run(ctx context.Context) {
	if a.Interval <= 0 {
		a.Interval = time.Minute
	}
	if a.Log == nil {
		a.Log = slog.Default()
	}
	log := a.Log.With("component", "announce")
	clients := map[string]*http.Client{}
	for _, family := range []string{"tcp4", "tcp6"} {
		d := &net.Dialer{Timeout: 10 * time.Second}
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.DialContext = func(ctx context.Context, _, addr string) (net.Conn, error) {
			return d.DialContext(ctx, family, addr)
		}
		clients[family] = &http.Client{Transport: tr, Timeout: 20 * time.Second}
	}
	last := map[string]string{} // the last outcome per family, to log changes only
	t := time.NewTicker(a.Interval)
	defer t.Stop()
	for {
		lan := a.LANURL()
		for _, family := range []string{"tcp4", "tcp6"} {
			outcome := "ok"
			if lan == "" {
				outcome = "no LAN address"
			} else if err := a.post(ctx, clients[family], lan); err != nil {
				outcome = err.Error()
			}
			if outcome != last[family] {
				switch {
				case outcome == "ok":
					log.Info("Announced to the directory", "family", family, "url", a.URL, "lan_url", lan)
				case family == "tcp6":
					// Normal on a network without IPv6.
					log.Debug("IPv6 announce failed", "err", outcome)
				default:
					log.Warn("Announce failed", "family", family, "err", outcome)
				}
				last[family] = outcome
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (a *Announcer) post(ctx context.Context, c *http.Client, lan string) error {
	body, _ := json.Marshal(announceMsg{Name: a.Name, LANURL: lan, Version: a.Version})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(a.URL, "/")+AnnouncePath,
		bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+a.Token())
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}
