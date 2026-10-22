#!/usr/bin/env bash
# import-accounts.sh — 通过接口批量导入 cockpit tools 导出的账号 JSON。
#
# 走 POST /panel/api/import/cockpit（multipart/form-data，字段名必须是 file）。
# 默认先 dry-run 只做分类，确认后再带 --commit 真正写盘入池。
#
# 用法:
#   ./import-accounts.sh accounts.json                      # 仅预览（dry-run）
#   ./import-accounts.sh accounts.json --commit             # 真正导入
#   ./import-accounts.sh accounts.json --commit --overwrite # 覆盖同 UID 已有账号
#   ./import-accounts.sh a.json --config /path/config.json
#   WB2A_URL=http://host:7863 WB2A_KEY=sk-xxx ./import-accounts.sh a.json --commit
#
# JSON 必须是账号数组；每条至少含 uid / access_token / refresh_token。
set -euo pipefail

cfg=${WB2A_CONFIG:-config.json}
file=""
commit=0
overwrite=0
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
    --commit) commit=1; shift ;;
    --overwrite) overwrite=1; shift ;;
    -h|--help) sed -n '2,14p' "$0"; exit 0 ;;
    -*) echo "未知参数: $1" >&2; exit 2 ;;
    *) file=$1; shift ;;
  esac
done

if [[ -z $file || ! -f $file ]]; then
  echo "用法: $0 <accounts.json> [--commit] [--overwrite]" >&2
  exit 2
fi

cfg_field() {
  python3 - "$cfg" "$1" <<'PY'
import json, sys
try:
    with open(sys.argv[1], encoding='utf-8') as f:
        cfg = json.load(f)
except Exception:
    sys.exit(1)
print(str(cfg.get(sys.argv[2]) or '').strip())
PY
}

# listen_to_url 把 config.json 的 listen 值转成 curl 可用的基地址。
# 直接拼 "http://$listen" 对 ":7863" 会得到 "http://:7863"（空 host），
# curl 报 CURLE_URL_MALFORMAT——默认配置下脚本必然失败。监听在 0.0.0.0 /
# [::] 时本机访问应改走回环地址。与 export-accounts.sh 保持同一实现。
listen_to_url() {
  local v=$1
  v=${v//[[:space:]]/}
  [[ -z $v ]] && return 0
  if [[ $v != *://* ]]; then
    v="http://$v"
  fi
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

if [[ -z $key && -f $cfg ]]; then key=$(cfg_field api_key || true); fi
if [[ -z $url && -f $cfg ]]; then
  url=$(listen_to_url "$(cfg_field listen || true)")
fi
url=${url:-http://127.0.0.1:7863}
url=${url%/}

if [[ -z $key ]]; then
  echo "找不到管理密钥：请在 config.json 设置 api_key，或用 WB2A_KEY=... 传入" >&2
  exit 1
fi

args=(-sS -H "Authorization: Bearer $key" -F "file=@${file}")
if [[ $commit -eq 0 ]]; then args+=(-F "dry_run=true"); fi
if [[ $overwrite -eq 1 ]]; then args+=(-F "overwrite=true"); fi

# 响应体写临时文件而不是固定 resp.json：固定名会覆盖工作目录里同名的用户文件，
# 并发执行两个导入也会互相踩踏。trap 保证异常退出也清理。
resp=$(mktemp "${TMPDIR:-/tmp}/wb2a-import.XXXXXX")
trap 'rm -f "$resp"' EXIT

code=$(curl "${args[@]}" -o "$resp" -w '%{http_code}' "$url/panel/api/import/cockpit") || {
  echo "请求失败：$url/panel/api/import/cockpit" >&2; exit 1; }

if [[ $code != 200 ]]; then
  echo "导入失败（HTTP ${code}）：$(head -c 300 "$resp")" >&2
  exit 1
fi

python3 - "$resp" "$commit" <<'PY'
import json, sys
d = json.load(open(sys.argv[1], encoding='utf-8'))
commit = sys.argv[2] == '1'
print(('已导入' if commit else '预览（未写盘）') + '：' +
      f"共 {d.get('total', 0)} · 新增 {d.get('added', 0)} · 更新 {d.get('updated', 0)} · "
      f"跳过 {d.get('skipped', 0)} · 无效 {d.get('invalid', 0)}")
for e in d.get('errors') or []:
    print('  - ' + e)
PY
