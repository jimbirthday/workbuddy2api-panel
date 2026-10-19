#!/bin/bash
# 批量签到脚本：遍历 auths/ 下所有 workbuddy-*.json 账号
# 用法: ./signin.sh [auths_dir]
set -e
cd "$(dirname "$0")"

BIN=./signin_bin
# 不仅「不存在」时构建：源码或 go.mod 比二进制新时必须重建，否则升级源码后
# 仍会执行旧的签到/额度逻辑（用户看到的是「代码改了但行为没变」）。
if [ ! -x "$BIN" ] || [ -n "$(find cmd/signin internal go.mod go.sum -newer "$BIN" -print -quit 2>/dev/null)" ]; then
    echo "build signin_bin ..."
    go build -o "$BIN" ./cmd/signin
fi

exec "$BIN" "${1:-auths}"
