package bootstrap

import (
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"fly-wireguard-vpn-proxy/internal/config"
	"fly-wireguard-vpn-proxy/internal/ui"

	"github.com/skip2/go-qrcode"
)

const (
	// startupWindow is how long we force keepalive on startup, to give
	// clients a chance to connect before we allow idle suspend.
	startupWindow = 2 * time.Minute

	// maxIdle is how long we allow WireGuard to be idle (no handshakes)
	// before we stop the keepalive ping and let Fly suspend the machine.
	maxIdle = 5 * time.Minute

	// interval is how often we check WG status and ping the proxy.
	interval = 30 * time.Second
)

type Server struct {
	cfg config.Config
}

func NewServer(cfg config.Config) Server {
	return Server{cfg: cfg}
}

func (s Server) Listen() {
	mux := http.NewServeMux()

	mux.HandleFunc("/", s.root)
	mux.HandleFunc("/healthz", s.healthz)
	mux.HandleFunc("/bootstrap", s.bootstrap)

	// Background keepalive loop:
	// - For the first 2 minutes after start, always send keepalive pings so
	//   the machine doesn't suspend before clients connect.
	// - After that, only continue pings while WireGuard has recent handshakes.
	//   If all peers have been idle for >5 minutes, stop pinging so Fly can
	//   auto-suspend the machine.
	if s.cfg.EndpointHost != "" && strings.ToLower(config.Getenv("KEEPALIVE_ENABLED", "true")) != "false" {
		go s.keepaliveLoop(s.cfg.EndpointHost)
	}

	addr := "0.0.0.0:" + s.cfg.Port
	log.Printf("bootstrap-http listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

func (s Server) root(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("This app only serves /bootstrap (one-time WireGuard config + QR)."))
}

func (s Server) healthz(w http.ResponseWriter, r *http.Request) {
	if _, err := os.Stat(s.cfg.PeerConfigPath()); err == nil {
		w.Write([]byte("ok"))
		return
	}
	http.Error(w, "config not ready", 503)
}

func (s Server) bootstrap(w http.ResponseWriter, r *http.Request) {
	if _, err := os.Stat(s.cfg.BootstrapDonePath()); err == nil {
		http.Error(w, "bootstrap already completed", 410)
		return
	}

	if s.cfg.BootstrapToken != "" &&
		r.URL.Query().Get("token") != s.cfg.BootstrapToken {
		http.Error(w, "unauthorized", 401)
		return
	}

	confBytes, err := os.ReadFile(s.cfg.PeerConfigPath())
	if err != nil {
		http.Error(w, "config not ready", 503)
		return
	}

	confStr := s.rewriteEndpoint(
		string(confBytes),
		s.cfg.EndpointHost,
		s.cfg.EndpointPort,
	)

	qrPNG, _ := qrcode.Encode(confStr, qrcode.Medium, 256)
	qrBase64 := base64.StdEncoding.EncodeToString(qrPNG)

	_ = os.WriteFile(s.cfg.BootstrapDonePath(),
		[]byte(time.Now().Format(time.RFC3339)),
		0o600,
	)

	ui.Page.Execute(w, map[string]any{
		"Config":   confStr,
		"QRBase64": qrBase64,
	})
}

// keepaliveLoop periodically pings the Fly proxy to keep the machine alive
// as long as there is active WireGuard traffic.
func (s Server) keepaliveLoop(appName string) {
	url := fmt.Sprintf("https://%s.fly.dev", appName)
	client := &http.Client{Timeout: 5 * time.Second}
	start := time.Now()
	wgInterface := config.Getenv("WG_INTERFACE", "wg0")

	log.Printf("keepalive: starting loop for %s (interval=%s, startup=%s, max_idle=%s, iface=%s)",
		url, interval, startupWindow, maxIdle, wgInterface)

	// lastIdle lets us detect when idle time "resets" (a new handshake),
	// which we treat as evidence that a client is actively connected.
	var lastIdle time.Duration = -1

	// Track an inferred "session" window based on continuous activity.
	var connected bool
	var connectedSince time.Time

	for {
		time.Sleep(interval)

		// During the startup window we always send pings, but we still log
		// a heartbeat so you can see activity.
		if time.Since(start) <= startupWindow {
			log.Printf("keepalive: tick (startup window), sending ping to %s", url)
		} else {
			// After the startup window, only continue if WireGuard is "recently active".
			idle, noHandshake, err := getWireGuardIdleDuration(wgInterface)
			if err != nil {
				// If we can't read WG status, log and continue; better to keep alive
				// than flap the machine due to transient errors.
				log.Printf("keepalive: tick, error checking wg status: %v (still sending ping)", err)
			} else if noHandshake {
				// Never seen a handshake on this interface; no session to attribute.
				if connected {
					// Defensive: end any inferred session.
					session := time.Since(connectedSince)
					log.Printf("keepalive: WireGuard has never seen a handshake; ending session (duration=%s) and allowing suspend",
						formatDuration(session))
				} else {
					log.Printf("keepalive: WireGuard has never seen a handshake; stopping keepalive to allow suspend")
				}
				return
			} else {
				roundedIdle := idle.Round(time.Second)

				// If idle decreased since the last tick, we saw a fresh handshake.
				// That strongly suggests a client is actively connected.
				if lastIdle >= 0 && idle < lastIdle {
					log.Printf("keepalive: handshake detected, idle reset from %s to %s",
						lastIdle.Round(time.Second), roundedIdle)
				}
				lastIdle = idle

				if idle > maxIdle {
					if connected {
						session := time.Since(connectedSince)
						log.Printf("keepalive: tick, status=disconnected, idle=%s (max %s); ending session duration=%s and stopping keepalive to allow suspend",
							roundedIdle, maxIdle, formatDuration(session))
					} else {
						log.Printf("keepalive: tick, status=disconnected, idle=%s (max %s); stopping keepalive to allow suspend",
							roundedIdle, maxIdle)
					}
					return
				}

				// We are within the idle threshold, so we infer a client is connected.
				if !connected {
					connected = true
					connectedSince = time.Now()
					log.Printf("keepalive: tick, status=connected, idle=%s (max %s); starting session at %s",
						roundedIdle, maxIdle, connectedSince.Format(time.RFC3339))
				} else {
					session := time.Since(connectedSince)
					log.Printf("keepalive: tick, status=connected, idle=%s (max %s); session_duration=%s; sending ping to %s",
						roundedIdle, maxIdle, formatDuration(session), url)
				}
			}
		}

		resp, err := client.Get(url)
		if err != nil {
			log.Printf("keepalive: ping failed: %v", err)
			continue
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
}

// getWireGuardIdleDuration returns the duration since the last handshake
// of the most recently active peer, plus a flag indicating if there has
// never been a handshake.
func getWireGuardIdleDuration(iface string) (time.Duration, bool, error) {
	out, err := exec.Command("wg", "show", iface, "latest-handshakes").Output()
	if err != nil {
		return 0, false, err
	}

	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	var lastHandshake int64

	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		ts, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			continue
		}
		if ts > lastHandshake {
			lastHandshake = ts
		}
	}

	if lastHandshake == 0 {
		// No handshakes ever.
		return 0, true, nil
	}

	return time.Since(time.Unix(lastHandshake, 0)), false, nil
}


// formatDuration renders a duration as a compact "XdYhZmWs" string so we can
// quickly eyeball how long a client has been (inferred) connected.
func formatDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}

	secs := int64(d.Seconds())
	days := secs / 86400
	secs %= 86400
	hours := secs / 3600
	secs %= 3600
	mins := secs / 60
	secs %= 60

	parts := make([]string, 0, 4)
	if days > 0 {
		parts = append(parts, fmt.Sprintf("%dd", days))
	}
	if hours > 0 {
		parts = append(parts, fmt.Sprintf("%dh", hours))
	}
	if mins > 0 {
		parts = append(parts, fmt.Sprintf("%dm", mins))
	}
	if secs > 0 || len(parts) == 0 {
		parts = append(parts, fmt.Sprintf("%ds", secs))
	}

	return strings.Join(parts, "")
}
// rewriteEndpoint normalizes the Endpoint line so that the config uses a
// client-usable value:
//
//   - If appName and port are known, we set: "Endpoint = <app>.fly.dev:port".
//   - Otherwise, if the existing Endpoint uses a bare IPv6 host without
//     brackets (e.g. "2a02:...:51820"), we rewrite it to "[ipv6]:port".
func (s Server) rewriteEndpoint(conf, appName, port string) string {
	lines := strings.Split(conf, "\n")

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "Endpoint ") && !strings.HasPrefix(trimmed, "Endpoint=") {
			continue
		}

		// Preserve original indentation.
		indentLen := len(line) - len(strings.TrimLeft(line, " \t"))
		indent := line[:indentLen]

		// Parse current value after "Endpoint" and optional "=".
		rest := strings.TrimSpace(strings.TrimPrefix(trimmed, "Endpoint"))
		rest = strings.TrimLeft(rest, " =")
		current := rest

		// Case 1: we know the app name and port -> use <app>.fly.dev:port.
		if appName != "" && port != "" {
			target := fmt.Sprintf("Endpoint = %s.fly.dev:%s", appName, port)
			lines[i] = indent + target
			return strings.Join(lines, "\n")
		}

		// Case 2: best-effort IPv6 fix when we don't know app name.
		// If the current value looks like "2a02:...:51820" (multiple ':' and
		// no brackets), wrap the host in [ ] to make it a valid Endpoint.
		if strings.Count(current, ":") > 1 && !strings.Contains(current, "]") {
			lastColon := strings.LastIndex(current, ":")
			if lastColon > 0 && lastColon < len(current)-1 {
				host := current[:lastColon]
				p := current[lastColon+1:]
				if p != "" {
					target := fmt.Sprintf("Endpoint = [%s]:%s", host, p)
					lines[i] = indent + target
					return strings.Join(lines, "\n")
				}
			}
		}

		// If we got here, either appName/port are missing and it wasn't a bare
		// IPv6 host, or parsing failed; leave the line as-is.
		return conf
	}

	// No Endpoint line found; nothing to normalize.
	return conf
}