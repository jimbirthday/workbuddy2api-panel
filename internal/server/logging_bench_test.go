package server

import (
	"io"
	"strings"
	"testing"
	"time"
)

// legacyReadForBench 是优化前的 ReadString 实现，保留为对照基准：
// 每行分配一个 string，再复制成 []byte 暂存。
func (s *chatStatsReader) legacyReadForBench(p []byte) (int, error) {
	if len(s.pend) > 0 {
		n := copy(p, s.pend)
		s.pend = s.pend[n:]
		return n, nil
	}
	line, err := s.br.ReadString('\n')
	if line != "" {
		s.parseSSELine(line)
		s.pend = []byte(line)
		n := copy(p, s.pend)
		s.pend = s.pend[n:]
		return n, nil
	}
	return 0, err
}

func benchReadAll(b *testing.B, read func(*chatStatsReader, []byte) (int, error)) {
	const frames = 500
	var sb strings.Builder
	for i := 0; i < frames; i++ {
		sb.WriteString(`data: {"id":"x","choices":[{"delta":{"content":"hello"}}]}`)
		sb.WriteString("\n\n")
	}
	src := sb.String()
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s := newChatStatsReaderSince(strings.NewReader(src), time.Now())
		buf := make([]byte, 32*1024)
		for {
			_, err := read(s, buf)
			if err == io.EOF {
				break
			}
			if err != nil {
				b.Fatal(err)
			}
		}
	}
}

func BenchmarkChatStatsReaderSSELegacyRead(b *testing.B) {
	benchReadAll(b, func(s *chatStatsReader, p []byte) (int, error) { return s.legacyReadForBench(p) })
}

func BenchmarkChatStatsReaderSSERead(b *testing.B) {
	benchReadAll(b, func(s *chatStatsReader, p []byte) (int, error) { return s.Read(p) })
}
