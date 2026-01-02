# regtest で試す（安全 / 無料）

mainnet の前に、`regtest` で Quote → Pay → Result の流れを試してください。

ルートは 2 つあります:

- A) 自動（integration tests）: テストが `bitcoind`（regtest）+ 2x `lnd` を自動起動します
- B) 手動（devnet）: `./scripts/devnet` をローカルで起動し、手元で操作します

## 前提（Nix なし）

- Go 1.24.4+
- `bitcoind` / `bitcoin-cli` が `PATH` にあること
- `lnd` / `lncli` が `PATH` にあること

## A) 自動（integration tests）

```sh
cd go-lcpd

LCP_ITEST_REGTEST=1 go test ./itest/e2e -run Regtest_LNDPayment -count=1 -v
LCP_ITEST_REGTEST=1 go test ./itest/e2e -run Regtest_LNDCustomMessages -count=1 -v
LCP_ITEST_REGTEST=1 go test ./itest/e2e -run RequesterGRPC -count=1 -v
```

詳細: `regtest-integration-ja.md`

## B) 手動（devnet）

### 1) devnet を起動（bitcoind + 2x lnd）

```sh
cd go-lcpd
./scripts/devnet up
./scripts/devnet status
```

初回のみ（対話的）:

```sh
./scripts/devnet lncli alice create
./scripts/devnet lncli bob create
```

### 2) 資金投入 / チャネル / スモークチェック

手動手順は長くなるため、`devnet-ja.md` を参照してください。

### 3) go-lcpd を起動 → oneshot 実行

Provider 設定（YAML）とプロファイルについては `configuration-ja.md` を参照してください。

## データ/ログとクリーンアップ（破壊的）

- devnet の state/log: `./.data/devnet/`

最初から作り直す（破壊的）:

```sh
cd go-lcpd
./scripts/devnet down
rm -rf ./.data/devnet
```
