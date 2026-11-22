package pgop

import (
	"errors"
	"regexp"
	"strings"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// maxSQLBytes 限制单条 SQL 长度，避免把超大参数塞进 argv / 客户端。
const maxSQLBytes = 64 * 1024

// nullMarker 是 psql `-P null=<marker>` 使用的 NULL 占位符。
//
// 选 SOH（0x01）的理由：CSV 模式下 NULL 与空串都渲染成空字段，只有自定义 null 串
// 才能区分「NULL」与「”」；0x01 在文本数据里几乎不可能出现，且不含逗号/引号/换行，
// 不会被 CSV 加引号。注意它只用于解析，绝不进响应。
const nullMarker = "\x01"

// readOnlyLeadingKeywords 是允许的语句首关键字（大小写不敏感）。
var readOnlyLeadingKeywords = map[string]struct{}{
	"SELECT":  {},
	"SHOW":    {},
	"EXPLAIN": {},
	"WITH":    {},
}

// explainInnerKeywords 是 EXPLAIN 真正允许分析的内层语句：只读集合的闭包。
// EXECUTE / CALL / INSERT / UPDATE / DELETE 都不得出现在 EXPLAIN 之后——
// EXPLAIN ANALYZE <写语句> 会**真的执行**写操作。
var explainInnerKeywords = map[string]struct{}{
	"SELECT": {},
	"WITH":   {},
	"VALUES": {},
	"TABLE":  {},
}

// dataModifyingRE 是「数据修改」关键字：只在 WITH（或 EXPLAIN 的 WITH 内层）里扫描。
//
// 为什么只在这里扫：SELECT / SHOW 是叶子只读语句，PostgreSQL 不允许在子查询里写数据
// （数据修改语句只能出现在 CTE 里），所以 SELECT 里出现的 update/delete 只能是标识符；
// 而 `WITH x AS (UPDATE t ... RETURNING *) SELECT * FROM x` 是真实写路径，必须拦。
// 扫描对象是去掉字符串与注释后的文本，普通标识符不会被当成关键字。
var dataModifyingRE = regexp.MustCompile(`\b(INSERT|UPDATE|DELETE|MERGE)\b`)

// selectIntoRE 拦住 `SELECT ... INTO <新表>`：它有 SELECT 前缀但实际建表。
var selectIntoRE = regexp.MustCompile(`\bINTO\b`)

// lockClauseRE 单独拦住加锁读：`FOR UPDATE` 已被 UPDATE 覆盖，这里补齐 SHARE 变体。
var lockClauseRE = regexp.MustCompile(`\bFOR\s+(NO\s+KEY\s+)?(UPDATE|SHARE|KEY\s+SHARE)\b`)

// forbiddenFunctionsRE 是拒绝的「只读外壳下的副作用」函数：阻塞、读写文件、
// 改会话/序列、发信号、操纵 WAL。它们都能通过 SELECT 前缀绕过关键字检查。
var forbiddenFunctionsRE = regexp.MustCompile(`(?i)\b(` +
	`pg_sleep|pg_sleep_for|pg_sleep_until|` +
	`pg_read_file|pg_read_binary_file|pg_ls_dir|pg_stat_file|pg_ls_waldir|` +
	`lo_import|lo_export|lo_create|lo_unlink|lo_put|lowrite|lo_from_bytea|` +
	`dblink|dblink_exec|dblink_connect|dblink_connect_u|` +
	`pg_terminate_backend|pg_cancel_backend|pg_reload_conf|pg_rotate_logfile|` +
	`pg_promote|pg_switch_wal|pg_create_restore_point|pg_logical_emit_message|` +
	`pg_backup_start|pg_backup_stop|pg_start_backup|pg_stop_backup|` +
	`set_config|nextval|setval` +
	`)\s*\(`)

// ValidateReadOnlySQL 执行只读白名单校验：只允许单条 SELECT / SHOW / EXPLAIN / WITH。
//
// 防的是 `psql -c` 会执行分号分隔的多条语句这一点：字符串/注释之外出现任何分号
// （末尾单个分隔符除外）一律拒绝；同时拒绝 psql 元命令（反斜杠）、写关键字、
// 加锁读、SELECT INTO、阻塞与副作用函数。
func ValidateReadOnlySQL(sql string) *pluginapi.Error {
	if strings.TrimSpace(sql) == "" {
		return invalid("sql 不得为空")
	}
	if len(sql) > maxSQLBytes {
		return invalid("sql 超过 64 KiB 上限")
	}
	if strings.ContainsRune(sql, 0) {
		return invalid("sql 含 NUL 字节")
	}
	sanitized, err := sanitizeSQL(sql)
	if err != nil {
		return invalid("sql 非法: " + err.Error())
	}
	statement := strings.TrimSpace(sanitized)
	if statement == "" {
		return invalid("sql 不得为空")
	}
	// 允许且仅允许一个结尾分号；其余分号都视为多语句。
	if strings.HasSuffix(statement, ";") {
		statement = strings.TrimSpace(strings.TrimSuffix(statement, ";"))
	}
	if strings.Contains(statement, ";") {
		return invalid("只允许单条只读语句，禁止多条语句（分号只能出现在末尾）")
	}
	leading := firstKeyword(statement)
	if _, ok := readOnlyLeadingKeywords[leading]; !ok {
		return invalid("只读白名单拒绝: 语句必须以 SELECT/SHOW/EXPLAIN/WITH 开头")
	}
	scannable := statement
	if leading == "EXPLAIN" {
		inner := stripExplainPrefix(statement[len(leading):])
		innerLeading := firstKeyword(inner)
		if _, ok := explainInnerKeywords[innerLeading]; !ok {
			return invalid("只读白名单拒绝: EXPLAIN 只允许分析 SELECT/WITH/VALUES/TABLE（EXPLAIN ANALYZE 会真的执行内层语句）")
		}
		scannable = inner
	}
	upper := strings.ToUpper(scannable)
	// 数据修改语句只能藏在 CTE 里：只对 WITH 系（含 EXPLAIN 的 WITH 内层）做写入扫描。
	scanWrites := leading == "WITH" || (leading == "EXPLAIN" && firstKeyword(scannable) == "WITH")
	if scanWrites {
		if match := dataModifyingRE.FindString(upper); match != "" {
			return invalid("只读白名单拒绝: WITH 子句含数据修改语句 " + match)
		}
	}
	if leading != "SHOW" {
		if selectIntoRE.MatchString(upper) {
			return invalid("只读白名单拒绝: 禁止 SELECT ... INTO（会建表）")
		}
	}
	if lockClauseRE.MatchString(strings.ToUpper(scannable)) {
		return invalid("只读白名单拒绝: 禁止加锁读 FOR UPDATE / FOR SHARE")
	}
	if match := forbiddenFunctionsRE.FindString(sanitizeSQLForFunctions(sql)); match != "" {
		// 报错用小写函数名（真实写法），而不是内部大写化的文本。
		name := strings.ToLower(strings.TrimSpace(strings.TrimSuffix(match, "(")))
		return invalid("只读白名单拒绝: 禁止副作用/阻塞函数 " + name)
	}
	return nil
}

// stripExplainPrefix 跳过 EXPLAIN 的选项前缀：`(ANALYZE, BUFFERS)` 括号块与裸
// ANALYZE / VERBOSE 关键字，返回内层语句文本。括号不闭合时返回空串（调用方判非法）。
func stripExplainPrefix(rest string) string {
	for {
		rest = strings.TrimLeft(rest, " \t\r\n")
		if rest == "" {
			return ""
		}
		if rest[0] == '(' {
			depth, i := 0, 0
			for ; i < len(rest); i++ {
				switch rest[i] {
				case '(':
					depth++
				case ')':
					depth--
				}
				if depth == 0 {
					break
				}
			}
			if depth != 0 {
				return ""
			}
			rest = rest[i+1:]
			continue
		}
		word, after := firstWord(rest)
		switch strings.ToUpper(word) {
		case "ANALYZE", "VERBOSE":
			rest = after
			continue
		default:
			return rest
		}
	}
}

// sanitizeSQL 去掉字符串字面量、引号标识符、美元引号与注释，返回仅剩 SQL 结构的文本。
// 字面量内容替换为空格，避免把字符串里的关键字或分号误判为真实语句。
func sanitizeSQL(sql string) (string, error) {
	var b strings.Builder
	b.Grow(len(sql))
	for i := 0; i < len(sql); {
		c := sql[i]
		switch {
		case c == '\'':
			next, err := skipQuoted(sql, i, '\'', isEscapeString(sql, i))
			if err != nil {
				return "", err
			}
			i = next
			b.WriteByte(' ')
		case c == '"':
			next, err := skipQuoted(sql, i, '"', false)
			if err != nil {
				return "", err
			}
			i = next
			b.WriteByte(' ')
		case c == '$':
			next, ok, err := skipDollarQuoted(sql, i)
			if err != nil {
				return "", err
			}
			if ok {
				i = next
				b.WriteByte(' ')
				continue
			}
			b.WriteByte(c)
			i++
		case c == '-' && i+1 < len(sql) && sql[i+1] == '-':
			for i < len(sql) && sql[i] != '\n' {
				i++
			}
			b.WriteByte(' ')
		case c == '/' && i+1 < len(sql) && sql[i+1] == '*':
			next, err := skipBlockComment(sql, i)
			if err != nil {
				return "", err
			}
			i = next
			b.WriteByte(' ')
		case c == '\\':
			return "", errors.New("禁止反斜杠 psql 元命令（如 \\g、\\!）")
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String(), nil
}

// sanitizeSQLForFunctions 返回「去掉注释与引号标识符、但保留字符串字面量」的文本。
//
// 函数名扫描要看到真实标识符：sanitizeSQL 会把字符串换成空格（正好避免把
// 'set_config(' 这类字面量误判），因此这里直接复用 sanitizeSQL 的结果，只在
// 它失败时退回原文——两边都不会放过真实函数调用。
func sanitizeSQLForFunctions(sql string) string {
	sanitized, err := sanitizeSQL(sql)
	if err != nil {
		return sql
	}
	return sanitized
}

// isEscapeString 判断位置 start 的单引号是否属于 E'...' 转义字符串（反斜杠有转义语义）。
func isEscapeString(sql string, start int) bool {
	if start == 0 {
		return false
	}
	prev := sql[start-1]
	if prev != 'e' && prev != 'E' {
		return false
	}
	// 前面是 e 但属于更长的标识符（如 table_e'x'）时不算前缀。
	return start < 2 || !isIdentByte(sql[start-2])
}

// isIdentByte 判断是否为标识符字符（用于区分 E 前缀与标识符结尾）。
func isIdentByte(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '$'
}

// skipQuoted 跳过一段引号内容，返回结束位置。
//
// 单引号/双引号内的成对引号表示一个字面引号；反斜杠转义只在 E'...' 里生效
// （standard_conforming_strings=on 时普通字符串里的反斜杠是普通字符）。
func skipQuoted(sql string, start int, quote byte, escapeBackslash bool) (int, error) {
	for i := start + 1; i < len(sql); i++ {
		switch sql[i] {
		case '\\':
			if escapeBackslash {
				i++
			}
		case quote:
			if i+1 < len(sql) && sql[i+1] == quote {
				i++
				continue
			}
			return i + 1, nil
		}
	}
	return 0, errors.New("引号未闭合")
}

// skipDollarQuoted 识别并跳过 $tag$ ... $tag$；不是美元引号时返回 ok=false。
func skipDollarQuoted(sql string, start int) (int, bool, error) {
	j := start + 1
	if j < len(sql) && sql[j] == '$' {
		// 空标签 $$ ... $$
	} else if j < len(sql) && (isLetter(sql[j]) || sql[j] == '_') {
		for j < len(sql) && (isIdentByte(sql[j]) && sql[j] != '$') {
			j++
		}
		if j >= len(sql) || sql[j] != '$' {
			return 0, false, nil
		}
	} else {
		return 0, false, nil
	}
	tag := sql[start : j+1]
	body := sql[j+1:]
	end := strings.Index(body, tag)
	if end < 0 {
		return 0, true, errors.New("美元引号未闭合")
	}
	return j + 1 + end + len(tag), true, nil
}

// skipBlockComment 跳过 PostgreSQL 支持的**可嵌套**块注释，返回注释结束位置。
func skipBlockComment(sql string, start int) (int, error) {
	depth := 0
	for i := start; i < len(sql); {
		switch {
		case i+1 < len(sql) && sql[i] == '/' && sql[i+1] == '*':
			depth++
			i += 2
		case i+1 < len(sql) && sql[i] == '*' && sql[i+1] == '/':
			depth--
			i += 2
			if depth == 0 {
				return i, nil
			}
		default:
			i++
		}
	}
	return 0, errors.New("块注释未闭合")
}

// firstKeyword 取语句的首个 ASCII 字母串并大写。
func firstKeyword(s string) string {
	s = strings.TrimLeft(s, " \t\r\n")
	i := 0
	for i < len(s) {
		if isLetter(s[i]) {
			i++
			continue
		}
		break
	}
	return strings.ToUpper(s[:i])
}

// firstWord 取首个词与剩余文本（用于剥离 EXPLAIN 选项）。
func firstWord(s string) (string, string) {
	s = strings.TrimLeft(s, " \t\r\n")
	i := 0
	for i < len(s) && !isSpace(s[i]) {
		i++
	}
	return s[:i], s[i:]
}

// isLetter 判断 ASCII 字母。
func isLetter(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }

// isSpace 判断空白。
func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\r' || c == '\n' }
