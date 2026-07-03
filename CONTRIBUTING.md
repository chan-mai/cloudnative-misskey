# コントリビュートガイド

cloud-native-misskeyへの貢献に感謝します。PRを送る前に以下を確認してください。

## ライセンスと権利許諾

- 本プロジェクトは**AGPL-3.0**([LICENSE](LICENSE))です。あなたのContributionも同ライセンスで配布されます。
- 加えて、コントリビュータは[Contributor License Agreement (CLA)](CLA.md)への同意が必要です。
  - 個人での貢献: [CLA.md](CLA.md)
  - 会社の業務としての貢献: [CLA-corporate.md](CLA-corporate.md)も必要

## DCO sign-off

全コミットに[Developer Certificate of Origin](https://developercertificate.org/)のsign-offを付けてください。

```bash
git commit -s -m "feat: ..."
```

`-s`で`Signed-off-by: 名前 <email>`トレーラが付きます。sign-offをもってCLAへの同意とみなします。`user.name`/`user.email`が本人のものであることを確認してください。

## 開発フロー

```bash
make manifests   # CRD/RBAC再生成
make generate    # DeepCopy再生成(hack/boilerplate.go.txtのAGPLヘッダが付与される)
make build       # bin/manager
make fmt vet     # 整形と静的検査
go test ./...    # テスト
make run         # kubeconfigのクラスタに対してローカル実行
```

PR前のチェック:

- `make fmt vet`と`go test ./...`が通ること
- 新規Goファイルの先頭に[hack/boilerplate.go.txt](hack/boilerplate.go.txt)のAGPLライセンスヘッダがあること(`make generate`/`make manifests`経由なら自動付与)
- CRDのspecを変えたら`make manifests`で`config/crd`を再生成し、READMEのspec表も更新すること

## コミット/PR規約

- コミットメッセージは`feat:`/`enhance:`/`fix:`/`docs:`/`chore:`等のprefixを付け、要点を簡潔に
- 1PR1トピック。無関係な整形差分を混ぜない
