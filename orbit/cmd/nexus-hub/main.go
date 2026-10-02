// nexus-hub: the waveshare board's hub on the LAN (see pkg/hub).
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/grandcat/zeroconf"
	"nexus-orbit/pkg/hub"
)

const version = "0.1.0"

func main() {
	home, _ := os.UserHomeDir()
	listen := flag.String("listen", ":8380", "address to listen on")
	tokenFile := flag.String("token-file", filepath.Join(home, ".config", "nexus", "hub_tokens"),
		"board tokens, one per line (the board's hub token must be one of them)")
	webDir := flag.String("web", "", "dashboard build to serve (waveshare-dashboard: npm run build -- --mode device, then dist/)")
	mdns := flag.Bool("mdns", true, "advertise _nexus-hub._tcp so boards find the hub")
	mdnsIface := flag.String("mdns-iface", "", "interfaces to advertise on, comma-separated (default: the LAN ones)")
	geminiKeyFile := flag.String("gemini-key-file", filepath.Join(home, ".config", "nexus", "gemini_key"),
		"Gemini API key for voice sessions (missing: no voice through the hub; boards stay direct)")
	mcpURL := flag.String("mcp-url", os.Getenv("NEXUS_MCP_URL"), "MCP server for voice tools (default $NEXUS_MCP_URL; empty = none)")
	mcpTokenFile := flag.String("mcp-token-file", filepath.Join(home, ".config", "nexus", "mcp_token"), "MCP bearer token")
	mcpMaxTools := flag.Int("mcp-max-tools", hub.DefaultMCPMaxTools, "most MCP tools offered to Gemini")
	music := flag.Bool("music", true, "run the music (queue, lookups, outputs) for the edges")
	invidiousURL := flag.String("invidious-url", os.Getenv("NEXUS_INVIDIOUS_URL"),
		"Invidious-compatible API (invidious-daemon) for music lookups, after MCP (default $NEXUS_INVIDIOUS_URL)")
	debug := flag.Bool("debug", false, "debug logging")
	flag.Parse()

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(log)

	tokens, err := readTokens(*tokenFile)
	if err != nil {
		log.Error("No board tokens", "file", *tokenFile, "err", err)
		fmt.Fprintf(os.Stderr, "Create it with one random token per line (chmod 600), and give the board the same token (POST /api/hub {\"token\": ...}).\n")
		os.Exit(1)
	}
	if *webDir != "" {
		if st, err := os.Stat(filepath.Join(*webDir, "index.html")); err != nil || st.IsDir() {
			log.Error("No index.html in the dashboard folder", "web", *webDir)
			os.Exit(1)
		}
	}

	geminiKey := readSecret(*geminiKeyFile)
	if geminiKey == "" {
		log.Warn("No Gemini key: voice sessions stay on the board", "file", *geminiKeyFile)
	}
	var mcp *hub.MCPClient
	if *mcpURL != "" {
		mcp = hub.NewMCPClient(*mcpURL, readSecret(*mcpTokenFile), *mcpMaxTools, log)
		// List the tools now, so the first session's setup doesn't wait for it.
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			mcp.Declarations(ctx, nil, false)
		}()
	}

	var tracks hub.TrackSource
	if *music {
		switch {
		case mcp != nil && *invidiousURL != "":
			tracks = hub.FallbackTracks{Primary: hub.MCPTracks{C: mcp}, Secondary: hub.NewInvidiousTracks(*invidiousURL)}
		case mcp != nil:
			tracks = hub.MCPTracks{C: mcp}
		case *invidiousURL != "":
			tracks = hub.NewInvidiousTracks(*invidiousURL)
		}
	}
	h := hub.New(hub.Config{Version: version, Tokens: tokens, WebDir: *webDir, Log: log,
		GeminiKey: geminiKey, MCP: mcp, Tracks: tracks})
	srv := &http.Server{Addr: *listen, Handler: h.Handler(), ReadHeaderTimeout: 10 * time.Second}

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Error("Listen failed", "addr", *listen, "err", err)
		os.Exit(1)
	}
	port := ln.Addr().(*net.TCPAddr).Port

	if *mdns {
		host, _ := os.Hostname()
		ifaces := lanInterfaces(*mdnsIface)
		names := make([]string, len(ifaces))
		for i, ifc := range ifaces {
			names[i] = ifc.Name
		}
		// An empty list would mean every interface to zeroconf.
		var zc *zeroconf.Server
		err := errors.New("no LAN interface")
		if len(ifaces) > 0 {
			zc, err = zeroconf.Register("nexus-hub-"+host, "_nexus-hub._tcp", "local.", port,
				[]string{"path=" + hub.BoardPath, "ver=" + version}, ifaces)
		}
		if err != nil {
			log.Warn("mDNS advertising failed; boards need hub_url", "err", err)
		} else {
			log.Info("mDNS: advertising _nexus-hub._tcp", "interfaces", strings.Join(names, ","))
			defer zc.Shutdown()
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
	}()

	log.Info("nexus-hub listening", "version", version, "port", port, "web", *webDir, "tokens", len(tokens),
		"voice", geminiKey != "", "mcp", mcp != nil, "music", trackSourceName(tracks))
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("Server failed", "err", err)
		os.Exit(1)
	}
}

// readSecret returns the first line of a file, "" if it can't be read.
func readSecret(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	line, _, _ := strings.Cut(string(data), "\n")
	return strings.TrimSpace(line)
}

// readTokens reads one token per line; blank lines and # comments are skipped.
func readTokens(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var tokens []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line != "" && !strings.HasPrefix(line, "#") {
			tokens = append(tokens, line)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(tokens) == 0 {
		return nil, errors.New("no tokens in the file")
	}
	return tokens, nil
}

// lanInterfaces is where the hub is advertised: the named interfaces, or
// else every interface that is up, does multicast and has a private IPv4
// address, without container, VPN and VM bridges (their addresses are of no
// use to a board, and the board takes the first address it sees).
func lanInterfaces(named string) []net.Interface {
	all, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []net.Interface
	for _, ifc := range all {
		if named != "" {
			for _, n := range strings.Split(named, ",") {
				if strings.TrimSpace(n) == ifc.Name {
					out = append(out, ifc)
				}
			}
			continue
		}
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagMulticast == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		skip := false
		for _, p := range []string{"docker", "br-", "veth", "virbr", "podman", "tailscale", "tun", "wg", "zt"} {
			if strings.HasPrefix(ifc.Name, p) {
				skip = true
			}
		}
		if skip {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil && ipn.IP.IsPrivate() {
				out = append(out, ifc)
				break
			}
		}
	}
	return out
}

func trackSourceName(t hub.TrackSource) string {
	if t == nil {
		return "off"
	}
	return t.Name()
}
