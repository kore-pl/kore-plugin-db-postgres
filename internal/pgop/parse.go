package pgop

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

const (
	// maxCellBytes 是单个单元格的字节上限（8 KiB，超出截断并置 truncated）。
	maxCellBytes = 8 * 1024
	// maxResultBytes 是整次查询结果的行数据总预算（8 MiB），防止「1000 行 × 多列 × 8 KiB」
	// 把常驻内存顶上去（G-03 单插件 ≤24 MiB）。
	maxResultBytes = 8 * 1024 * 1024
)

// parseCSV 把 psql `--csv` 输出解析成列名 + 单元格矩阵。
//
// 约定：首条记录是表头（psql 默认输出表头）；maxRows 限制数据行数，读满后再读到
// 记录即判定 truncated；单格超过 cellLimit 字节时截断该格并置 truncated。
// 多行文本、含逗号/引号的单元格由 RFC4180 规则自然处理。
func parseCSV(stdout []byte, maxRows, cellLimit int) (columns []string, rows [][]string, truncated bool, err error) {
	reader := csv.NewReader(bytes.NewReader(stdout))
	reader.FieldsPerRecord = -1
	reader.ReuseRecord = false
	header, readErr := reader.Read()
	if readErr != nil {
		if isCSVEOF(readErr) {
			return []string{}, [][]string{}, false, nil
		}
		return nil, nil, false, fmt.Errorf("解析 psql 输出表头失败: %w", readErr)
	}
	columns = normalizeColumns(header)
	rows = make([][]string, 0, 32)
	for len(rows) < maxRows {
		record, err := reader.Read()
		if err != nil {
			if isCSVEOF(err) {
				return columns, rows, truncated, nil
			}
			return nil, nil, false, fmt.Errorf("解析 psql 输出数据行失败: %w", err)
		}
		cells, wasCut := truncateCells(record, cellLimit)
		if wasCut {
			truncated = true
		}
		rows = append(rows, cells)
	}
	// 读满 maxRows 后还能读到一条记录，说明结果被行数上限截断。
	if _, err := reader.Read(); err == nil {
		truncated = true
	} else if !isCSVEOF(err) {
		return nil, nil, false, fmt.Errorf("解析 psql 输出尾部失败: %w", err)
	}
	return columns, rows, truncated, nil
}

// isCSVEOF 判断 csv.Reader 是否读到了正常结束。
func isCSVEOF(err error) bool { return errors.Is(err, io.EOF) }

// normalizeColumns 去重列名：同名列表头加 _2/_3 后缀，保证 JSON 对象键唯一。
func normalizeColumns(header []string) []string {
	columns := make([]string, len(header))
	seen := make(map[string]int, len(header))
	for i, name := range header {
		name = strings.TrimSpace(name)
		if name == "" {
			name = fmt.Sprintf("column_%d", i+1)
		}
		if n, dup := seen[name]; dup {
			seen[name] = n + 1
			name = fmt.Sprintf("%s_%d", name, n+1)
		}
		seen[name] = 1
		columns[i] = name
	}
	return columns
}

// truncateCells 按字节截断超长单元格，保证不切断 UTF-8 序列。
func truncateCells(record []string, cellLimit int) ([]string, bool) {
	cells := make([]string, len(record))
	cut := false
	for i, cell := range record {
		if len(cell) <= cellLimit {
			cells[i] = cell
			continue
		}
		cells[i] = truncateUTF8(cell, cellLimit)
		cut = true
	}
	return cells, cut
}

// truncateUTF8 截断到不超过 limit 字节，且不回退到半个多字节字符。
func truncateUTF8(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	end := limit
	for end > 0 && (s[end]&0xC0) == 0x80 {
		end--
	}
	return s[:end]
}

// buildRow 把一行单元格编码成 JSON 对象（列名 → 值）。
//
// 值推断规则（psql 文本，保持「所见即所得」）：
//   - nullMarker → null
//   - 合法 JSON 数字 → 数字（SELECT 1 AS n 得到 1，而不是 "1"）
//   - 其余 → 字符串（布尔按 psql 文本为 t/f，不做猜测性转换）
func buildRow(columns []string, cells []string) (json.RawMessage, error) {
	var buf bytes.Buffer
	buf.Grow(64)
	buf.WriteByte('{')
	for i := 0; i < len(cells) && i < len(columns); i++ {
		if i > 0 {
			buf.WriteByte(',')
		}
		key, err := json.Marshal(columns[i])
		if err != nil {
			return nil, err
		}
		buf.Write(key)
		buf.WriteByte(':')
		value, err := encodeCell(cells[i])
		if err != nil {
			return nil, err
		}
		buf.Write(value)
	}
	buf.WriteByte('}')
	return json.RawMessage(buf.Bytes()), nil
}

// encodeCell 把一个单元格编码成 JSON 值。
func encodeCell(cell string) ([]byte, error) {
	if cell == nullMarker {
		return []byte("null"), nil
	}
	if looksNumeric(cell) {
		return []byte(cell), nil
	}
	return json.Marshal(cell)
}

// looksNumeric 判断单元格是否是可以原样写进 JSON 的数字。
//
// 用 json.Valid 做最终判定，天然排除 "007"、"1 "、"1,000" 这类「看起来像数字但不是
// 合法 JSON 数字」的文本，避免产出非法 JSON 或把编号当数值。
func looksNumeric(cell string) bool {
	if cell == "" {
		return false
	}
	switch cell[0] {
	case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
	default:
		return false
	}
	return json.Valid([]byte(cell))
}

// buildRows 批量编码行，并在总字节超预算时截断（budget 按 decode 后的 JSON 字节计）。
func buildRows(columns []string, cells [][]string, budget int) ([]json.RawMessage, bool, error) {
	rows := make([]json.RawMessage, 0, len(cells))
	for _, record := range cells {
		row, err := buildRow(columns, record)
		if err != nil {
			return nil, false, err
		}
		if len(row) > budget {
			return rows, true, nil
		}
		budget -= len(row)
		rows = append(rows, row)
	}
	return rows, false, nil
}
