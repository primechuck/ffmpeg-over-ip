# Local install testing — Phase1 + short-circuit

This is how to test on your own machine / N100s without pushing to prod.

## 0. Build boring static binaries (like original)

```bash
git checkout phase1-multi-addr-busy   # or total-rework-wireguard for full RO+RW
CGO_ENABLED=0 go build -ldflags="-s -w" -o build/ffmpeg-over-ip-client ./cmd/client
CGO_ENABLED=0 go build -ldflags="-s -w" -o build/ffmpeg-over-ip-server ./cmd/server
ls -lh build/
./build/ffmpeg-over-ip-client --debug-print-search-paths
./build/ffmpeg-over-ip-server --debug-print-search-paths
```

## 1. Binary install (systemd) — single little node

```bash
sudo cp build/ffmpeg-over-ip-server /usr/local/bin/
sudo cp build/ffmpeg-over-ip-client /usr/local/bin/
sudo ln -sf /usr/local/bin/ffmpeg-over-ip-client /usr/local/bin/ffmpeg-test

# Get real jellyfin-ffmpeg (or use stub for test)
# For test without real ffmpeg:
gcc -O2 -o /usr/local/bin/ffmpeg tests/localstub/stub.c fio/fio.c -Ifio -lpthread
sudo chmod +x /usr/local/bin/ffmpeg
sudo cp /usr/local/bin/ffmpeg /usr/local/bin/ffprobe

# Config
sudo mkdir -p /etc/ffmpeg-over-ip
sudo tee /etc/ffmpeg-over-ip/server.jsonc <<'JSONC'
{
  "address": "0.0.0.0:5050",
  "authSecret": "test",
  "maxConcurrent": 1,
  "shortCircuitRead": ["/media"],
  "log": "stdout"
}
JSONC

# Systemd (from origin/feat/shared-storage)
sudo cp systemd/ffmpeg-over-ip-server.service /etc/systemd/system/ 2>/dev/null || \
  curl -sL https://raw.githubusercontent.com/primechuck/ffmpeg-over-ip/feat/shared-storage/systemd/ffmpeg-over-ip-server.service | sudo tee /etc/systemd/system/ffmpeg-over-ip-server.service

sudo systemctl daemon-reload
sudo systemctl enable --now ffmpeg-over-ip-server
sudo journalctl -u ffmpeg-over-ip-server -f
# Should log: listening on 0.0.0.0:5050, max concurrent: 1, short-circuit RO for: [/media]
```

## 2. Client as drop-in ffmpeg (Jellyfin)

```bash
# Jellyfin looks for ffmpeg in /usr/lib/jellyfin-ffmpeg/
# Replace it with our client wrapper
sudo cp build/ffmpeg-over-ip-client /usr/lib/jellyfin-ffmpeg/ffmpeg
sudo cp build/ffmpeg-over-ip-client /usr/lib/jellyfin-ffmpeg/ffprobe

# Env for Jellyfin process (in /etc/environment or systemd override)
export FFMPEG_OVER_IP_CLIENT_ADDRESS=10.100.0.2:5050,10.100.0.3:5050,10.100.0.4:5050,10.100.0.5:5050
export FFMPEG_OVER_IP_CLIENT_AUTH_SECRET=test
export FFMPEG_OVER_IP_CLIENT_DIAL_TIMEOUT=3s
export FFMPEG_OVER_IP_CLIENT_FALLBACK_TO_LOCAL=true

# Restart Jellyfin
sudo systemctl restart jellyfin
# Trigger a 4K transcode → logs should show "connected to 10.100.0.x"
```

## 3. Local dev test without Jellyfin (fake ffmpeg)

```bash
# Fake ffmpeg that sleeps 10s to simulate busy
cat > /tmp/ffmpeg <<'SH'
#!/bin/sh
echo "fake ffmpeg args: $@" >&2
sleep 10
echo "done" >&2
exit 0
SH
chmod +x /tmp/ffmpeg && cp /tmp/ffmpeg /tmp/ffprobe

# 2 servers, max 1 each, different ports
FFMPEG_OVER_IP_SERVER_ADDRESS=127.0.0.1:5051 FFMPEG_OVER_IP_SERVER_AUTH_SECRET=test \
FFMPEG_OVER_IP_SERVER_MAX_CONCURRENT=1 FFMPEG_OVER_IP_SERVER_LOG=stdout \
/tmp/ffmpeg-over-ip-server --config /dev/null 2>&1 | tee /tmp/srv1.log &
FFMPEG_OVER_IP_SERVER_ADDRESS=127.0.0.1:5052 FFMPEG_OVER_IP_SERVER_AUTH_SECRET=test \
FFMPEG_OVER_IP_SERVER_MAX_CONCURRENT=1 FFMPEG_OVER_IP_SERVER_LOG=stdout \
/tmp/ffmpeg-over-ip-server --config /dev/null 2>&1 | tee /tmp/srv2.log &

sleep 2

# Occupy both
FFMPEG_OVER_IP_CLIENT_ADDRESS=127.0.0.1:5051,127.0.0.1:5052 FFMPEG_OVER_IP_CLIENT_AUTH_SECRET=test \
/tmp/ffmpeg-over-ip-client -i /tmp/input.mp4 /tmp/out.ts > /tmp/long1.log 2>&1 &
FFMPEG_OVER_IP_CLIENT_ADDRESS=127.0.0.1:5051,127.0.0.1:5052 FFMPEG_OVER_IP_CLIENT_AUTH_SECRET=test \
/tmp/ffmpeg-over-ip-client -i /tmp/input.mp4 /tmp/out.ts > /tmp/long2.log 2>&1 &
sleep 2

# 3rd should get busy on both and failover log
FFMPEG_OVER_IP_CLIENT_ADDRESS=127.0.0.1:5051,127.0.0.1:5052 FFMPEG_OVER_IP_CLIENT_AUTH_SECRET=test \
FFMPEG_OVER_IP_CLIENT_LOG=stdout /tmp/ffmpeg-over-ip-client -i /tmp/input.mp4 /tmp/out.ts
# Expected:
# server 127.0.0.1:5051 busy, trying next (1 remaining)
# server 127.0.0.1:5052 busy, trying next (0 remaining)
# all servers busy/unreachable: busy: server busy: at capacity

pkill -f ffmpeg-over-ip-server
```

## 4. Docker (boring containers)

```bash
# Client
docker build -f Dockerfile.client -t ffmpeg-over-ip-client:boring .
docker run --rm -e FFMPEG_OVER_IP_CLIENT_ADDRESS=10.100.0.2:5050 -e FFMPEG_OVER_IP_CLIENT_AUTH_SECRET=test \
  ffmpeg-over-ip-client:boring -version

# Server (needs real ffmpeg in same dir)
docker build -f Dockerfile.server -t ffmpeg-over-ip-server:boring .
# Prod with real ffmpeg:
cat > Dockerfile.prod <<'DF'
FROM ffmpeg-over-ip-server:boring
COPY --from=jellyfin/jellyfin-ffmpeg:7.1 /usr/lib/jellyfin-ffmpeg/ffmpeg /opt/ffmpeg-over-ip/ffmpeg
COPY --from=jellyfin/jellyfin-ffmpeg:7.1 /usr/lib/jellyfin-ffmpeg/ffprobe /opt/ffmpeg-over-ip/ffprobe
DF
docker build -f Dockerfile.prod -t ffmpeg-over-ip-server:prod .

docker run -p 5050:5050 -e FFMPEG_OVER_IP_SERVER_ADDRESS=0.0.0.0:5050 \
  -e FFMPEG_OVER_IP_SERVER_AUTH_SECRET=test -e FFMPEG_OVER_IP_SERVER_MAX_CONCURRENT=1 \
  ffmpeg-over-ip-server:prod
```

## 5. Docker Compose demo (shared storage)

```bash
# From origin/feat/shared-storage
docker compose up --build
# server mounts shared-data:/media, client writes test file and tries transcode via stub
# Check logs: should show LOCAL vs TUNNEL via stub.c reporting
```

## 6. WireGuard mesh (4× N100s + orchestrator)

```bash
# On each host, wg-quick up wg0 (10.100.0.0/24) — see docs/wireguard-rework.md
# Server binds only to WG IP, only allows WG peers
FFMPEG_OVER_IP_SERVER_ADDRESS=10.100.0.2:5050
FFMPEG_OVER_IP_SERVER_ALLOWED_PEERS=10.100.0.0/24
FFMPEG_OVER_IP_SERVER_SHORT_CIRCUIT_READ=/media

# Client dials WG IPs
FFMPEG_OVER_IP_CLIENT_ADDRESS=10.100.0.2:5050,10.100.0.3:5050,10.100.0.4:5050,10.100.0.5:5050
```

## 7. Short-circuit verification with stub

```bash
gcc -O2 -o /tmp/ffmpeg tests/localstub/stub.c fio/fio.c -Ifio -lpthread
mkdir -p /tmp/shared/media && echo hello > /tmp/shared/media/test.txt

FFMPEG_OVER_IP_SERVER_ADDRESS=127.0.0.1:5055 FFMPEG_OVER_IP_SERVER_AUTH_SECRET=test \
FFMPEG_OVER_IP_SERVER_SHORT_CIRCUIT_READ=/tmp/shared/media FFMPEG_OVER_IP_SERVER_LOG=stdout \
./build/ffmpeg-over-ip-server &

FFMPEG_OVER_IP_CLIENT_ADDRESS=127.0.0.1:5055 FFMPEG_OVER_IP_CLIENT_AUTH_SECRET=test \
./build/ffmpeg-over-ip-client -i /tmp/shared/media/test.txt /tmp/out.ts
# Should log ro_hits=1 and stub prints LOCAL /tmp/shared/media/test.txt
```
