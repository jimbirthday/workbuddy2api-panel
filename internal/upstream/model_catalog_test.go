package upstream

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// TestModelCatalogPutPersists：put 落盘后可被重新加载（内存→文件→内存往返）。
func TestModelCatalogPutPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "model.json")
	c := &modelCatalog{path: path}
	c.put("test-model-x", ModelCapEntry{ContextLength: 123456, MaxOutputTokens: 4096, Source: "modelsdev"})

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("落盘文件读取失败: %v", err)
	}
	var got map[string]ModelCapEntry
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("落盘内容非法 JSON: %v", err)
	}
	if got["test-model-x"].ContextLength != 123456 {
		t.Fatalf("落盘条目不符: %+v", got["test-model-x"])
	}
	// 重新加载（新实例）应命中刚写的条目。
	c2 := &modelCatalog{path: path}
	if e, ok := c2.get("test-model-x"); !ok || e.ContextLength != 123456 {
		t.Fatalf("重新加载未命中: ok=%v e=%+v", ok, e)
	}
}

// TestModelCatalogConcurrentPut：并发 put 不同模型使用唯一临时名，不做锁内 I/O，
// 最终文件必须是完整合法 JSON 且包含全部条目（旧实现固定 ".tmp" 会互相截断）。
func TestModelCatalogConcurrentPut(t *testing.T) {
	path := filepath.Join(t.TempDir(), "model.json")
	c := &modelCatalog{path: path}

	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			c.put("m"+string(rune('a'+n%26))+string(rune('0'+n/26)), ModelCapEntry{
				ContextLength: int64(1000 + n), Source: "modelsdev",
			})
		}(i)
	}
	wg.Wait()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("并发落盘后文件读取失败: %v", err)
	}
	var got map[string]ModelCapEntry
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("并发落盘后文件损坏: %v", err)
	}
	// 每个并发 put 的模型都应在最终文件中（快照含当时全部条目）。
	if len(got) < 24 {
		t.Fatalf("最终文件条目数 %d < 24，说明有写入丢失/截断", len(got))
	}
}
