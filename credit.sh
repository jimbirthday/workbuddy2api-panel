#!/usr/bin/env bash
# credit.sh — WorkBuddy 积分日报（默认美化输出）
#
# 用法:
#   ./credit.sh            # 人类可读日报
#   ./credit.sh -json      # 原始 JSON
#
# 二进制升级: go build -o credit ./cmd/credit
set -euo pipefail
cd "$(dirname "$0")"
# 二进制兜底：与 login.sh / signin.sh 同款（README 承诺「脚本首次使用自动 go build」）。
if [[ ! -x ./credit ]]; then
    go build -o credit ./cmd/credit
fi
if [[ "${1:-}" == "-json" ]]; then
    exec ./credit
fi
exec ./credit -pretty
