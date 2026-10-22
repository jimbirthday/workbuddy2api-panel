#!/usr/bin/env bash
# export-accounts.sh — 手动导出账号运维清单（CSV），绕过面板「导出账号」按钮。
#
# 背景：面板按钮用 <a href> 直接打开 /panel/api/accounts/export，浏览器不会带
# Authorization 头，后端因此回 401 {"error":"invalid_api_key"}。本脚本用 curl
# 带上 Bearer 头请求同一个接口，把服务端脱敏后的 CSV 存到本地。
#
# 用法:
#   ./export-accounts.sh                     # 读 ./config.json 的 listen + api_key
#   ./export-accounts.sh --config /path/config.json
#   WB2A_URL=http://host:7863 WB2A_KEY=sk-xxx ./export-accounts.sh
#   ./export-accounts.sh -o /tmp/accounts.csv
#
# 只依赖 curl（+ 可选 python3 用于从 config.json 取值）。
set -euo pipefail

cfg=${WB2A_CONFIG:-config.json}
out=""
url=${WB2A_URL:-}
key=${WB2A_KEY:-}

# 需要取值的选项统一走 need_val：直接 `cfg=$2` 在缺值时会被 `set -u` 打成
# "$2: unbound variable"，用户只看到一行 shell 内部错误而不是用法提示。
need_val() {
  if [[ $# -lt 2 || -z $2 ]]; then
    echo "参数 $1 需要一个值（$0 --help 查看用法）" >&2
    exit 2
  fi
  printf '%s' "$2"
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --config) cfg=$(need_val "$1" "${2-}"); shift 2 ;;
    -o|--out) out=$(need_val "$1" "${2-}"); shift 2 ;;
    -h|--help) sed -n '2,14p' "$0"; exit 0 ;;
    *) echo "未知参数: $1" >&2; exit 2 ;;
  esac
done

cfg_field() {
  python3 - "$cfg" "$1" <<'PY'
import json, sys
try:
    with open(sys.argv[1], encoding='utf-8') as f:
        cfg = json.load(f)
except Exception:
    sys.exit(1)
v = cfg.get(sys.argv[2]) or ''
print(str(v).strip())
PY
}

# listen_to_url 把 config.json 的 listen 值（":7863" / "0.0.0.0:7863" /
# "127.0.0.1:7863" / 已带 scheme 的完整 URL）转成 curl 可用的基地址。
# 直接拼 "http://$listen" 对 ":7863" 会得到 "http://:7863"——空 host，
# curl 报 CURLE_URL_MALFORMAT，脚本此前在默认配置下必然失败。
# 监听在 0.0.0.0 / [::] 时本机访问应走回环地址。
listen_to_url() {
  local v=$1
  v=${v//[[:space:]]/}
  [[ -z $v ]] && return 0
  if [[ $v != *://* ]]; then
    v="http://$v"
  fi
  # 拆出 host 部分：scheme://host[:port][/path]
  local scheme=${v%%://*} rest host
  rest=${v#*://}
  host=${rest%%/*}
  if [[ $host == :* ]]; then
    v="$scheme://127.0.0.1$host"
  elif [[ $host == 0.0.0.0* ]]; then
    v="$scheme://127.0.0.1${host#0.0.0.0}"
  elif [[ $host == "[::]"* ]]; then
    v="$scheme://[::1]${host#\[::\]}"
  fi
  printf '%s' "$v"
}

if [[ -z $key && -f $cfg ]]; then
  key=$(cfg_field api_key || true)
fi
if [[ -z $url && -f $cfg ]]; then
  url=$(listen_to_url "$(cfg_field listen || true)")
fi
url=${url:-http://127.0.0.1:7863}
url=${url%/}

if [[ -z $key ]]; then
  echo "找不到管理密钥：请在 config.json 设置 api_key，或用 WB2A_KEY=... 传入" >&2
  exit 1
fi

if [[ -z $out ]]; then
  out="accounts-$(date +%Y%m%d-%H%M%S).csv"
fi

code=$(curl -sS -o "$out" -w '%{http_code}' \
  -H "Authorization: Bearer $key" \
  "$url/panel/api/accounts/export") || {
    echo "请求失败：$url/panel/api/accounts/export" >&2; exit 1; }

if [[ $code != 200 ]]; then
  echo "导出失败（HTTP ${code}）：$(head -c 300 "$out")" >&2
  echo "  检查 $cfg 的 api_key 是否与面板登录用的一致" >&2
  exit 1
fi

lines=$(awk 'END { print NR }' "$out")
echo "已导出 → ${out}（${lines} 行，首行为表头）"
