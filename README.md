# Personal WireGuard VPN on Fly.io

### Self-contained HTTPS bootstrap • No host dependencies • Auto-sleeping

Run a lightweight **personal WireGuard VPN** (for private browsing, travel, or unblocking networks) entirely on **Fly.io**.
This app uses the community-maintained [`linuxserver/docker-wireguard`](https://github.com/linuxserver/docker-wireguard) image and adds:

* A **built-in HTTPS bootstrap server** (QR code + raw config)
* A **volume-backed WireGuard config** with auto-generation
* **No container shells**, no local Docker, no Go toolchain
* **Auto-sleeping** when idle to reduce cost
* **One-time bootstrap** for simple and secure setup

> **Note:** Fly machines do *not* wake from UDP (WireGuard) traffic.
> Wake the instance by visiting any HTTPS endpoint (e.g. `/healthz` or `/bootstrap`).

---

## Features

| Feature                           | Description                                                                       |
| --------------------------------- | --------------------------------------------------------------------------------- |
| **Self-contained image**          | Builds WireGuard + bootstrap server in Fly’s builder—no local Docker/Go required. |
| **HTTPS bootstrap**               | Visit a single page to receive a QR code and `.conf` file.                        |
| **One-time bootstrap**            | `/bootstrap` works once; subsequent calls return HTTP 410.                        |
| **Volume-backed config**          | Keys and client configs persist across deploys.                                   |
| **Auto-sleeping**                 | Machine suspends when no WireGuard traffic is detected.                           |
| **Token protection (optional)**   | Add `?token=...` to restrict one-time setup.                                      |
| **No SSH or shell access needed** | All provisioning handled automatically.                                           |

---

## Prerequisites

* A Fly.io account
* `flyctl` installed and logged in (`fly auth login`)
* Basic familiarity with WireGuard on your device
* Comfort running a few CLI commands — **no container shell required**

---

# Quick Start

### 1. Clone

```bash
git clone <repo-url>
cd <repo>
```

### 2. Create the Fly app (using the included `fly.toml`)

```bash
fly launch --copy-config --no-deploy --generate-name
```

### 3. Create the configuration volume

```bash
fly volumes create config --size 1
```

WireGuard keys and peer configs live in `/config`.

### 4. Allocate a dedicated IPv4 (required)

Fly shared IPv4s **do not support UDP**. You must allocate a dedicated IPv4:

```bash
fly ips allocate-v4
```

### 5. (Optional but recommended) Set a bootstrap token

```bash
fly secrets set BOOTSTRAP_TOKEN='a-very-long-random-string'
```

### 6. Deploy

```bash
fly deploy
```

### 7. Bootstrap your client

Open in your browser:

```
https://<appname>.fly.dev/bootstrap?token=YOUR_BOOTSTRAP_TOKEN
```

* If you did **not** set a token, simply open `/bootstrap`.
* On first visit this will:

  * Display a QR code for the WireGuard mobile app
  * Show the full `.conf` text for desktop clients
  * Mark bootstrap as complete (`/config/bootstrap_done`)

### 8. Connect from WireGuard

Use your new configuration.
The VPN endpoint runs on UDP port **51820**.

---

# How It Works

### Architecture Overview

This project layers a small Go bootstrap server onto `linuxserver/wireguard`:

```
┌─────────────────────────┐
│ Fly App (single machine)│
├─────────────────────────┤
│ WireGuard service (UDP) │  ← from linuxserver/wireguard
│ Bootstrap HTTP server   │  ← custom binary (QR + config)
└─────────────────────────┘
```

### Dockerfile (multi-stage)

1. **Builder stage:**

   * Uses `golang:alpine` to build a static `bootstrap-http` binary.

2. **Final stage:**

   * Base image: `linuxserver/wireguard:latest`.
   * Adds sysctls (`ip_forward`, `src_valid_mark`) for routing.
   * Adds `bootstrap-http` as an s6 service.
   * Wraps `/init` via `unshare` for Fly compatibility.

### fly.toml

Defines:

* UDP **51820** → WireGuard
* HTTPS **443** → bootstrap server on internal **8081**
* Volume mount: `/config`
* Environment variables:

  * WireGuard:
    `PEERS=1`, `PEERDNS=auto`, `SERVERPORT=51820`, etc.
  * Bootstrap:
    `BOOTSTRAP_PORT=8081`, `BOOTSTRAP_PEER_NAME=peer1`,
    `KEEPALIVE_ENABLED=true`, `WG_INTERFACE=wg0`

### Bootstrap server behavior

* Blocks until `/config/<peer>/<peer>.conf` exists
* Routes:

  * `GET /healthz` → 200 once ready
  * `GET /bootstrap` → One-time page (QR + config)
* Writes `/config/bootstrap_done` to disable future bootstrapping

---

# Security Notes

* **Bootstrap page is served over HTTPS**, terminated by Fly.
* **WireGuard UDP traffic is end-to-end encrypted**, but not TLS-based.
* Using `BOOTSTRAP_TOKEN` ensures only holders of the token can complete the one-time setup.
* All private keys persist only on the Fly volume (`/config`).

This is intended as a **personal, convenience VPN**, not an anonymity service.

---

# Troubleshooting

### No handshake / “0 B received”

* The app may be **asleep** — visit `https://<app>.fly.dev/healthz`.
* Ensure you have a **dedicated IPv4** (shared IPv4s do not support UDP).
* If you recreated the volume or redeployed, keys changed → revisit `/bootstrap`.

### Port conflicts

* `linuxserver/wireguard` uses port 8080 internally.
* This project uses **8081** for the bootstrap server to avoid conflicts.

### Suspended machine

* Fly machines do not wake from UDP.
* Wake it by hitting any HTTPS endpoint.

---

# Configuration Reference

| Env Var                   | Default   | Purpose                                           |
| ------------------------- | --------- | ------------------------------------------------- |
| `BOOTSTRAP_PORT`          | `8081`    | Port for the bootstrap HTTP server                |
| `BOOTSTRAP_TOKEN`         | *(unset)* | Optional token required for `/bootstrap`          |
| `BOOTSTRAP_PEER_NAME`     | `peer1`   | Which peer config to present                      |
| `KEEPALIVE_ENABLED`       | `true`    | Ping Fly proxy to prevent suspension while active |
| `WG_INTERFACE`            | `wg0`     | Interface to monitor for WireGuard activity       |
| `BOOTSTRAP_ENDPOINT_PORT` | `51820`   | Override port in client config                    |

---

# Known Issues

* Fly outbound traffic may not exit using the instance’s public IP.
* Geo-location of Fly IPs may not match the chosen region.
* Machines do not wake on UDP traffic.