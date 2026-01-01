# regtest 統合テスト（bitcoind + lnd）

このドキュメントは、`TestIntegration_Regtest_LNDPayment`（Lightning 支払いの regtest 統合テスト）をローカルで実行する方法を説明します。

## 前提

- Go 1.24.4+
- `bitcoind` / `bitcoin-cli` が `PATH` にあること
- `lnd` / `lncli` が `PATH` にあること

## 実行

```sh
cd go-lcpd
export LCP_ITEST_REGTEST=1

# main test (invoice creation + payment)
go test ./itest/e2e -run Regtest_LNDPayment -count=1 -v

# optional coverage
go test ./itest/e2e -run Regtest_LNDCustomMessages -count=1 -v
go test ./itest/e2e -run RequesterGRPC -count=1 -v
go test ./itest/e2e -run Regtest -count=1 -v
```

## devnet（手動）

統合テストではなく、手動で regtest devnet（bitcoind + 2x lnd）を動かしたい場合は `./scripts/devnet` を使います。

手順: `devnet-ja.md`

### カスタムメッセージ（`lcp_manifest` 交換）

`TestIntegration_Regtest_LNDCustomMessages_LCPManifest` は、2 つの `lnd` ノードが BOLT #1 カスタムメッセージで `lcp_manifest` を交換でき、`ListLCPPeers` が 1 つのピアを観測できる（チャネル不要）ことを示す統合テストです。
上の「optional coverage」コマンドで実行できます。

## 生成データ/ログ

テストは `bitcoind` と `lnd` の状態/ログを `os.MkdirTemp` で作る一時ディレクトリに保存します。
通常はテスト終了時に削除されますが、以下の場合は残ります:

- `LCP_ITEST_KEEP_DATA=1` を設定した
- テストが失敗した

失敗時（または data を保持する設定時）、テストログに `keeping regtest data dir: ...` が表示されます。

## 実装メモ（安定化のためのノブ）

- `bitcoind` は `-listen=0`（P2P listen なし、RPC のみ）で起動します。
- `lnd` は ZMQ ポート競合を避けるため `--bitcoind.rpcpolling` を有効にします。

## lnd gRPC スタブについて

このプロジェクトは `github.com/lightningnetwork/lnd/lnrpc` を直接 import しません。
代わりに `go-lcpd/internal/lnd/lnrpc` と `go-lcpd/internal/lnd/routerrpc` の vendored Go スタブを使います。
これは protobuf の置換/フォークによる依存衝突を避け、`go test` を単純に保つためです。

更新するには、対象の lnd モジュールバージョンからスタブをコピー/置換してください:

```sh
cd go-lcpd
./scripts/update-lnd-stubs v0.19.3-beta
```
