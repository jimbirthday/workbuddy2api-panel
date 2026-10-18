package panel

import (
	"encoding/csv"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// exportLimit 是请求记录导出的硬上限，与 request_logs 的读取上限一致：
// 面板导出不能变成「一次把全部 JSONL 归档读进内存」的入口。
const exportLimit = 1000

// writeDownloadHeaders 设置下载响应头。文件名只由服务端生成，不接收用户输入，
// 因此不存在头部注入；面板下载的值全部来自已脱敏的请求/用量记录。
func writeDownloadHeaders(w http.ResponseWriter, contentType, filename string) {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
}

// exportFilename 生成带本地时间戳的下载名（仅 ASCII，避免不同客户端对
// filename* 的处理差异）。
func exportFilename(prefix, ext string) string {
	return fmt.Sprintf("%s-%s.%s", prefix, time.Now().Format("20060102-150405"), ext)
}

// csvText 对自由文本做最小防公式注入处理：Excel/Sheets 会把 = + - @ 开头的
// 单元格当公式执行。导出的是运维日志，不应因某个 UA 而触发客户端公式。
func csvText(v string) string {
	if v == "" {
		return ""
	}
	switch v[0] {
	case '=', '+', '-', '@':
		return "'" + v
	default:
		return v
	}
}

// writeCSV 写出 CSV；错误只影响本次下载，不改变任何服务端状态。
func writeCSV(w http.ResponseWriter, filename string, header []string, rows [][]string) {
	writeCSVRows(w, filename, header, func(yield func([]string) bool) {
		for _, row := range rows {
			if !yield(row) {
				return
			}
		}
	})
}

// writeCSVRows 逐行写出 CSV，调用方可以用回调把大结果流式产出而不必先组出
// 完整的二维切片。导出的请求记录上限 1000，但用量维度可能更多；流式写出让
// 峰值内存只与单行有关。
func writeCSVRows(w http.ResponseWriter, filename string, header []string, emit func(yield func([]string) bool)) {
	writeDownloadHeaders(w, "text/csv; charset=utf-8", filename)
	// UTF-8 BOM：Excel 直接双击打开时中文表头/原因才不会乱码。JSONL/JSON
	// 导出不写 BOM（会破坏标准解析器）。
	_, _ = w.Write([]byte{0xEF, 0xBB, 0xBF})
	cw := csv.NewWriter(w)
	_ = cw.Write(header)
	if emit != nil {
		emit(func(row []string) bool { return cw.Write(row) == nil })
	}
	cw.Flush()
}

// parseExportLimit 解析导出条数；缺省与非法值回落到 request_logs 的默认 200，
// 上限 1000。与列表接口保持同一口径，导出结果不会比页面看到的多。
func parseExportLimit(raw string) int {
	limit := 200
	if v := strings.TrimSpace(raw); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > exportLimit {
		limit = exportLimit
	}
	return limit
}
