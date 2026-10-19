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
# 源码或依赖比二进制新时也重建，避免升级后仍拿旧 credit 查企业额度/积分口径。
if [[ ! -x ./credit ]] || [[ -n "$(find cmd/credit internal go.mod go.sum -newer ./credit -print -quit 2>/dev/null)" ]]; then
    go build -o credit ./cmd/credit
fi
if [[ "${1:-}" == "-json" ]]; then
    exec ./credit
fi
exec ./credit -pretty
