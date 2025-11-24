# Stage 1: build the bootstrap HTTP+QR server inside the Fly builder
FROM golang:1.22-alpine AS builder

# Need git so Go can fetch module dependencies
RUN apk add --no-cache git

WORKDIR /src

# Copy the entire project source
COPY . .

# Fetch dependencies
RUN go mod tidy

# Build a static Linux binary
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -o /out/bootstrap-http ./cmd/bootstrap-http

# Stage 2: runtime image based on linuxserver/wireguard
FROM linuxserver/wireguard:latest

# Make sure we have the full util-linux unshare, not just BusyBox, so we can
# use the PID-namespace workaround linuxserver images often need for s6-overlay.
RUN apk add --no-cache util-linux

# Normally with docker, you would set these sysctls via the run command, but fly.io isn't really docker
# We also add network optimizations for streaming:
# - BBR congestion control for better throughput/latency
# - Larger UDP buffers for WireGuard performance
# - Optimized TCP settings
RUN printf '\necho "Writing sysctl settings"\n\
sysctl -w net.ipv4.conf.all.src_valid_mark=1\n\
sysctl -w net.ipv4.ip_forward=1\n\
sysctl -w net.core.default_qdisc=fq\n\
sysctl -w net.ipv4.tcp_congestion_control=bbr\n\
sysctl -w net.core.rmem_max=4194304\n\
sysctl -w net.core.wmem_max=4194304\n\
sysctl -w net.ipv4.udp_rmem_min=8192\n\
sysctl -w net.ipv4.udp_wmem_min=8192\n' >> /etc/s6-overlay/s6-rc.d/init-wireguard-confs/run

# Copy bootstrap HTTP binary from builder stage
COPY --from=builder /out/bootstrap-http /usr/local/bin/bootstrap-http
RUN chmod +x /usr/local/bin/bootstrap-http

# Disable s6-overlay cron service to allow Fly.io machine to sleep on idle
RUN rm -f /etc/s6-overlay/s6-rc.d/user/contents.d/svc-cron

# Set MTU to 1280 to avoid fragmentation issues on Fly.io network
ENV WG_MTU=1280

# Simple entrypoint script:
# - enable IP forwarding and NAT so the instance actually routes VPN traffic
# - start bootstrap-http (listening on 0.0.0.0:8081) in the background
# - then exec unshare --pid --fork --mount-proc /init so s6-overlay runs as
#   PID 1 in its own PID namespace, as required.
RUN printf '#!/bin/sh\nset -e\n\n# Ensure we have a MASQUERADE rule on egress so traffic from 10.13.13.0/24\n# can reach the internet and replies know how to get back.\nif ! iptables -t nat -C POSTROUTING -o eth0 -j MASQUERADE 2>/dev/null; then\n  iptables -t nat -A POSTROUTING -o eth0 -j MASQUERADE\nfi\n\n# Default policy for FORWARD chain is often DROP in docker/container environments.\n# We need to allow forwarding for the VPN traffic to pass through.\niptables -P FORWARD ACCEPT\n\n# Start the bootstrap HTTP server in the background\n/usr/local/bin/bootstrap-http &\n\n# Hand over to s6-overlay / WireGuard stack in its own PID namespace\nexec unshare --pid --fork --mount-proc /init\n' \
    > /docker-entrypoint.sh && chmod +x /docker-entrypoint.sh

ENTRYPOINT ["/docker-entrypoint.sh"]