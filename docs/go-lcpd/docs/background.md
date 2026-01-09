# Background run + logging (lcpd-grpcd)

This doc describes practical ways to:

- run `lcpd-grpcd` in the background
- keep logs on disk (with basic rotation)

Note: logs are sensitive. `lcpd-grpcd` is designed to avoid logging raw prompts/outputs, but logs still contain metadata
(peer ids, call ids, prices, timings). See [Logging & privacy](/go-lcpd/docs/logging).

For long-running infrastructure, use a service manager (systemd/launchd).
For "keep running after I close my terminal", the repo script is usually enough.

## Option A (recommended for quick use): repo script + log file

The repo ships a small runner script:

```sh
cd go-lcpd
./scripts/lcpd-grpcd --help
```

### 0) Build the daemon binary (one-time)

```sh
cd go-lcpd
mkdir -p bin
GOBIN="$PWD/bin" go install ./tools/lcpd-grpcd
```

### 1) Configure environment

You can keep startup parameters and secrets out of your shell history by using
direnv (`.envrc`, recommended). If you don't use direnv, export `LCPD_*` env vars
in your shell or service manager configuration.

#### Option A: direnv (`.envrc`)

```sh
cd go-lcpd
cp .envrc.sample .envrc
direnv allow
```

Then edit `.envrc` to add your `LCPD_*` settings.

Requester-only (no Provider execution) tip:

- If you run from a directory containing `config.yaml`, `lcpd-grpcd` will use it by default.
- This repo's `go-lcpd/config.yaml` keeps the Provider disabled by default.
- If you have a different `config.yaml` that enables the Provider (or you're running from another working directory),
  you can force Provider disabled by setting `LCPD_PROVIDER_CONFIG_PATH=/dev/null` (empty YAML).

### 2) Start / stop

Start (background):

```sh
cd go-lcpd
./scripts/lcpd-grpcd up
```

Status:

```sh
cd go-lcpd
./scripts/lcpd-grpcd status
```

Tail logs:

```sh
cd go-lcpd
./scripts/lcpd-grpcd logs
```

Stop (graceful SIGINT):

```sh
cd go-lcpd
./scripts/lcpd-grpcd down
```

### Sanity checks

Quick status checks:

```sh
cd go-lcpd
./scripts/lcpd-grpcd status
tail -n 50 ./.data/lcpd-grpcd/logs/lcpd-grpcd.log
```

Port-level checks (pick one):

```sh
nc -vz 127.0.0.1 50051
```

```sh
lsof -nP -iTCP:50051 -sTCP:LISTEN
```

If you override the listen address (e.g. `LCPD_GRPCD_GRPC_ADDR=127.0.0.1:15051`), adjust the port accordingly.

### Logs + rotation behavior

- Log link path default: `./.data/lcpd-grpcd/logs/lcpd-grpcd.log` (gitignored)
- PID file: `./.data/lcpd-grpcd/pids/lcpd-grpcd.pid`
- At each startup, a new dated log file is created (e.g. `lcpd-grpcd.YYYYmmdd-HHMMSS.<pid>.<rand>.log`).
- `lcpd-grpcd.log` is a symlink pointing at the current run's log file.
- Only the newest `LCPD_GRPCD_LOG_KEEP_ROTATED` run logs are kept (default 10).

## Option B: systemd (Linux)

systemd gives you:

- auto-restart on crashes
- boot-time startup
- robust log retention via journald

Minimal user service example (recommended when `lnd` certs/macaroons live under your home directory):

1) Write `~/.config/systemd/user/lcpd-grpcd.service`:

```ini
[Unit]
Description=lcpd-grpcd (Lightning Compute Protocol daemon)
After=network-online.target

[Service]
Type=simple
WorkingDirectory=/ABS/PATH/TO/go-lcpd
ExecStart=/usr/bin/env direnv exec /ABS/PATH/TO/go-lcpd /ABS/PATH/TO/go-lcpd/bin/lcpd-grpcd -grpc_addr=127.0.0.1:50051
Restart=on-failure
RestartSec=2s
KillSignal=SIGINT

[Install]
WantedBy=default.target
```

2) Start + enable:

```sh
systemctl --user daemon-reload
systemctl --user enable --now lcpd-grpcd
```

3) Logs:

```sh
journalctl --user -u lcpd-grpcd -f
```

To keep user services running across reboots without an active login session, you may need:

```sh
loginctl enable-linger "$USER"
```

## Option C: launchd (macOS)

launchd is the macOS service manager. For background + auto-restart, use a LaunchAgent (runs as your user).

1) Create `~/Library/LaunchAgents/com.bruwbird.lcpd-grpcd.plist` (adjust absolute paths):

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
  <dict>
    <key>Label</key>
    <string>com.bruwbird.lcpd-grpcd</string>

    <key>ProgramArguments</key>
    <array>
      <string>/bin/bash</string>
      <string>-lc</string>
      <string>cd /ABS/PATH/TO/go-lcpd && exec direnv exec . ./bin/lcpd-grpcd -grpc_addr=127.0.0.1:50051</string>
    </array>

    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>

    <key>StandardOutPath</key>
    <string>/ABS/PATH/TO/go-lcpd/.data/lcpd-grpcd/logs/lcpd-grpcd.launchd.out.log</string>
    <key>StandardErrorPath</key>
    <string>/ABS/PATH/TO/go-lcpd/.data/lcpd-grpcd/logs/lcpd-grpcd.launchd.err.log</string>
  </dict>
</plist>
```

2) Load it:

```sh
launchctl bootstrap "gui/$UID" ~/Library/LaunchAgents/com.bruwbird.lcpd-grpcd.plist
launchctl enable "gui/$UID/com.bruwbird.lcpd-grpcd"
launchctl kickstart -k "gui/$UID/com.bruwbird.lcpd-grpcd"
```

3) Inspect status:

```sh
launchctl print "gui/$UID/com.bruwbird.lcpd-grpcd"
```

Notes:

- The plist uses `direnv exec` so you can keep credentials out of the plist itself (store secrets in `.envrc`).
- macOS does not rotate these log files automatically; use `newsyslog` or periodically archive/delete old logs.
