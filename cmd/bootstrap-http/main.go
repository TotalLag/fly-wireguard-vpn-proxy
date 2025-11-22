package main

import (
	"encoding/base64"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/skip2/go-qrcode"
)

const (
	configDir       = "/config"
	defaultPeerName = "peer1"
)

var pageTmpl = template.Must(template.New("page").Parse(`<!doctype html>
<html>
  <head>
    <meta charset="utf-8">
    <title>Your WireGuard VPN</title>
    <style>
      body { font-family: system-ui, -apple-system, BlinkMacSystemFont, sans-serif; max-width: 800px; margin: 2rem auto; padding: 0 1rem; }
      pre { background: #f5f5f5; padding: 1rem; overflow-x: auto; }
      img { border: 1px solid #ddd; padding: 0.5rem; background: #fff; max-width: 100%; height: auto; }
    </style>
  </head>
  <body>
    <h1>Your WireGuard VPN</h1>

    <h2>1. Scan this QR code with the WireGuard mobile app</h2>
    <p>Open the WireGuard app on your phone and choose "Scan from QR code".</p>
    <img src="data:image/png;base64,{{.QRBase64}}" alt="WireGuard config QR">

    <h2>2. Or copy this configuration into a desktop client</h2>
    <pre>{{.Config}}</pre>

    <p><strong>Note:</strong> This page is one-time only. After you close it, the bootstrap endpoint is disabled.</p>
  </body>
</html>
`))

type pageData struct {
	Config   string
	QRBase64 string
}

func main() {
	bootstrapToken := os.Getenv("BOOTSTRAP_TOKEN")
	port := getenv("BOOTSTRAP_PORT", "8080")
	peerName := getenv("BOOTSTRAP_PEER_NAME", defaultPeerName)

	confPath := filepath.Join(configDir, peerName, peerName+".conf")
	donePath := filepath.Join(configDir, "bootstrap_done")

	// Endpoint normalization for QR payload:
	// - Use the public Fly hostname (<app>.fly.dev) if FLY_APP_NAME is set
	// - Use SERVERPORT (WireGuard server port) or override via BOOTSTRAP_ENDPOINT_PORT
	endpointHost := os.Getenv("FLY_APP_NAME")
	endpointPort := getenv("BOOTSTRAP_ENDPOINT_PORT", getenv("SERVERPORT", "51820"))

	mux := http.NewServeMux()

	// Simple landing page so hitting the root URL doesn't look "broken".
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("This app only serves /bootstrap (one-time WireGuard config + QR)."))
	})

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if fileExists(confPath) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok\n"))
			return
		}
		http.Error(w, "config not ready", http.StatusServiceUnavailable)
	})

	mux.HandleFunc("/bootstrap", func(w http.ResponseWriter, r *http.Request) {
		if fileExists(donePath) {
			http.Error(w, "bootstrap already completed", http.StatusGone)
			return
		}

		// Optional token auth for the one-time bootstrap.
		if bootstrapToken != "" {
			token := r.URL.Query().Get("token")
			if token == "" {
				http.Error(w, "missing token", http.StatusUnauthorized)
				return
			}
			if token != bootstrapToken {
				http.Error(w, "invalid token", http.StatusForbidden)
				return
			}
		}

		confBytes, err := os.ReadFile(confPath)
		if err != nil {
			log.Printf("read conf: %v", err)
			http.Error(w, "config not ready", http.StatusServiceUnavailable)
			return
		}
		confStr := string(confBytes)

		// Normalize Endpoint so the QR encodes a client-usable value.
		// In particular, we rewrite to "<app>.fly.dev:port" when we know the app name,
		// since the auto-detected SERVERURL inside the container may not be ideal.
		confStr = rewriteEndpoint(confStr, endpointHost, endpointPort)

		// Generate a PNG QR code whose payload is exactly the WireGuard config text.
		// This is what the WireGuard app expects when scanning a QR to import a tunnel.
		qrPNG, err := qrcode.Encode(confStr, qrcode.Medium, 256)
		if err != nil {
			log.Printf("qr encode: %v", err)
			http.Error(w, "failed to generate qr", http.StatusInternalServerError)
			return
		}
		qrBase64 := base64.StdEncoding.EncodeToString(qrPNG)

		if err := os.WriteFile(donePath, []byte(time.Now().Format(time.RFC3339)), 0o600); err != nil {
			log.Printf("write done: %v", err)
			http.Error(w, "failed to finalize bootstrap", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := pageTmpl.Execute(w, pageData{
			Config:   confStr,
			QRBase64: qrBase64,
		}); err != nil {
			log.Printf("template execute: %v", err)
		}
	})

	addr := "0.0.0.0:" + port
	log.Printf("bootstrap-http listening on %s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("listen: %v", err)
	}
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func waitForFile(path string, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fileExists(path) {
			return
		}
		time.Sleep(time.Second)
	}
	log.Printf("warning: config file %s not found after %s", path, timeout)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// rewriteEndpoint normalizes the Endpoint line so that the config uses a
// client-usable value:
//
//   - If appName and port are known, we set: "Endpoint = <app>.fly.dev:port".
//   - Otherwise, if the existing Endpoint uses a bare IPv6 host without
//     brackets (e.g. "2a02:...:51820"), we rewrite it to "[ipv6]:port".
func rewriteEndpoint(conf, appName, port string) string {
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