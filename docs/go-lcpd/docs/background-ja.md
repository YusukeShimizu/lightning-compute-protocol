# バックグラウンド実行 + ログ（lcpd-grpcd）

このドキュメントでは、実用的な方法として以下を説明します:

- `lcpd-grpcd` をバックグラウンドで動かす
- ログをディスクに残す（簡易ローテーション付き）

注意: ログはセンシティブです。`lcpd-grpcd` は生のプロンプト/出力をログに残さない設計ですが、ログにはメタデータ（peer id / job id / 価格 / 時間など）が残ります。詳細は [ログとプライバシー](/go-lcpd/docs/logging-ja) を参照してください。

長期運用のインフラでは service manager（systemd/launchd）を使ってください。
「ターミナルを閉じても動き続けてほしい」程度なら、このリポジトリのスクリプトで十分な場合が多いです。

## Option A（手軽で推奨）: リポジトリのスクリプト + ログファイル

このリポジトリには小さな runner スクリプトがあります:

```sh
cd go-lcpd
./scripts/lcpd-grpcd --help
```

### 0) daemon バイナリをビルド（初回のみ）

```sh
cd go-lcpd
mkdir -p bin
GOBIN="$PWD/bin" go install ./tools/lcpd-grpcd
```

### 1) env ファイルを作る（推奨）

起動パラメータと秘密情報をシェル履歴に残さないためです。

```sh
cd go-lcpd
cp lcpd.env.sample lcpd.env
chmod 600 lcpd.env
```

Requester のみ（Provider 実行なし）のヒント:

- `config.yaml` があるディレクトリで実行すると、`lcpd-grpcd` はデフォルトでそれを使います。
- このリポジトリの `go-lcpd/config.yaml` は、デフォルトで Provider を無効にしています。
- Provider を有効にする別の `config.yaml` を使っている（または別の作業ディレクトリで実行している）場合は、
  `LCPD_PROVIDER_CONFIG_PATH=/dev/null`（空 YAML）を指定して Provider 無効を強制できます。

### 2) 起動 / 停止

起動（バックグラウンド）:

```sh
cd go-lcpd
./scripts/lcpd-grpcd up
```

ステータス:

```sh
cd go-lcpd
./scripts/lcpd-grpcd status
```

ログを tail:

```sh
cd go-lcpd
./scripts/lcpd-grpcd logs
```

停止（graceful SIGINT）:

```sh
cd go-lcpd
./scripts/lcpd-grpcd down
```

### 疎通チェック

簡易チェック:

```sh
cd go-lcpd
./scripts/lcpd-grpcd status
tail -n 50 ./.data/lcpd-grpcd/logs/lcpd-grpcd.log
```

ポートレベルのチェック（どちらか）:

```sh
nc -vz 127.0.0.1 50051
```

```sh
lsof -nP -iTCP:50051 -sTCP:LISTEN
```

### ログとローテーションの挙動

- ログのリンクパス（デフォルト）: `./.data/lcpd-grpcd/logs/lcpd-grpcd.log`（gitignored）
- PID ファイル: `./.data/lcpd-grpcd/pids/lcpd-grpcd.pid`
- 起動ごとに日時付きのログファイルを作成します（例: `lcpd-grpcd.YYYYmmdd-HHMMSS.<pid>.<rand>.log`）。
- `lcpd-grpcd.log` は、現在の run のログファイルを指す symlink です。
- `LCPD_GRPCD_LOG_KEEP_ROTATED` 個の最新 run ログだけを保持します（デフォルト 10）。

## Option B: systemd（Linux）

systemd でできること:

- クラッシュ時の自動再起動
- 起動時の自動起動
- journald による堅牢なログ保持

最小の user service 例（`lnd` の cert/macaroon が home 配下にある場合に推奨）:

1) `~/.config/systemd/user/lcpd-grpcd.service` を作成:

```ini
[Unit]
Description=lcpd-grpcd (Lightning Compute Protocol daemon)
After=network-online.target

[Service]
Type=simple
WorkingDirectory=/ABS/PATH/TO/go-lcpd
EnvironmentFile=/ABS/PATH/TO/go-lcpd/lcpd.env
ExecStart=/ABS/PATH/TO/go-lcpd/bin/lcpd-grpcd -grpc_addr=127.0.0.1:50051
Restart=on-failure
RestartSec=2s
KillSignal=SIGINT

[Install]
WantedBy=default.target
```

2) 起動 + enable:

```sh
systemctl --user daemon-reload
systemctl --user enable --now lcpd-grpcd
```

3) ログ:

```sh
journalctl --user -u lcpd-grpcd -f
```

ログインしていない状態でも user service を動かし続けたい場合、以下が必要になることがあります:

```sh
loginctl enable-linger "$USER"
```

## Option C: launchd（macOS）

launchd は macOS の service manager です。バックグラウンド + 自動再起動には LaunchAgent（ユーザーとして起動）を使います。

1) `~/Library/LaunchAgents/com.bruwbird.lcpd-grpcd.plist` を作成（絶対パスは調整）:

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
      <string>cd /ABS/PATH/TO/go-lcpd && source ./lcpd.env && exec ./bin/lcpd-grpcd -grpc_addr=127.0.0.1:50051</string>
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

2) ロード:

```sh
launchctl bootstrap "gui/$UID" ~/Library/LaunchAgents/com.bruwbird.lcpd-grpcd.plist
launchctl enable "gui/$UID/com.bruwbird.lcpd-grpcd"
launchctl kickstart -k "gui/$UID/com.bruwbird.lcpd-grpcd"
```

3) ステータス確認:

```sh
launchctl print "gui/$UID/com.bruwbird.lcpd-grpcd"
```

補足:

- plist では `source ./lcpd.env` を使うことで、認証情報を plist に埋め込まずに済みます。
- macOS はこれらのログファイルを自動ローテーションしません。`newsyslog` を使うか、定期的にアーカイブ/削除してください。
