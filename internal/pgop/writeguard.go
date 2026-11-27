package pgop

import (
	"strings"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// 写入分两级确认（与 db-mysql.exec 同口径，见 kore-plugin-db-mysql/internal/mysqlop/writeguard.go）：
//   - data（INSERT/UPDATE/DELETE/MERGE）：confirm=true 即可，属于日常增删改；
//   - ddl （DROP/TRUNCATE/ALTER/CREATE/RENAME/GRANT/REVOKE/REINDEX/CLUSTER/VACUUM 等）：
//     confirm=true 之外还必须手打首两个关键字（如 "DROP TABLE"），把不可回滚动作
//     从「点一下」抬到「想清楚再打一遍」。
const (
	// ExecKindData 表示数据行级写入（可回滚/可重放代价低）。
	ExecKindData = "data"
	// ExecKindDDL 表示结构/权限/会话级写入（不可回滚或影响面大）。
	ExecKindDDL = "ddl"
)

// maxExecRows 限制写语句可能返回的结果集行数（RETURNING、VACUUM SUMMARY 等）。
const maxExecRows = 100

// dataWriteKeywords 是数据行级写语句的首关键字白名单。
// 与 MySQL 的差异：REPLACE 不是 PG 关键字（去掉），MERGE 是 PG 15+ 的合法写语句（加入）。
var dataWriteKeywords = map[string]struct{}{
	"INSERT": {},
	"UPDATE": {},
	"DELETE": {},
	"MERGE":  {},
}

// ddlWriteKeywords 是结构/权限/会话级语句的首关键字白名单。
// PG 方言调整：去掉 MySQL 专有的 FLUSH/OPTIMIZE/REPAIR/CALL，加入 PG 的
// REINDEX/CLUSTER/VACUUM/ANALYZE（ANALYZE 无 ANALYZE 前缀时是独立的统计更新语句）
// 与 SECURITY LABEL / COMMENT 等边角结构语句由首关键字白名单统一拒绝（fail-closed）。
var ddlWriteKeywords = map[string]struct{}{
	"DROP":     {},
	"TRUNCATE": {},
	"ALTER":    {},
	"CREATE":   {},
	"RENAME":   {},
	"GRANT":    {},
	"REVOKE":   {},
	"REINDEX":  {},
	"CLUSTER":  {},
	"VACUUM":   {},
	"ANALYZE":  {},
	"SET":      {},
	"RESET":    {},
	"COMMENT":  {},
}

// ValidateWriteSQL 执行写入白名单 + 两级确认校验，返回语句分级（data / ddl）。
//
// 与 ValidateReadOnlySQL 共用 sanitizeSQL 的结构清洗：字符串/美元引号/注释内不算关键字、
// 只允许单条语句、拒绝 NUL 与反斜杠 psql 元命令、拒绝 64 KiB 以上语句。
// 另外拒绝 PG 特有的侧信道：COPY ... FROM PROGRAM / COPY ... TO PROGRAM（可执行任意系统命令）、
// pg_read_file/lo_import/dblink 等在只读 sqlguard 里已经列为副作用函数。
func ValidateWriteSQL(sql string, confirm bool, confirmText string) (string, *pluginapi.Error) {
	if strings.TrimSpace(sql) == "" {
		return "", invalid("请填写要执行的 SQL 语句，不能为空")
	}
	if len(sql) > maxSQLBytes {
		return "", invalid("SQL 语句超过 64 KiB，请缩短后再试")
	}
	if strings.ContainsRune(sql, 0) {
		return "", invalid("SQL 语句含 NUL 空字符，请去掉后重试")
	}
	sanitized, err := sanitizeSQL(sql)
	if err != nil {
		return "", invalid("SQL 语句格式有误，请检查: " + err.Error())
	}
	statement := strings.TrimSpace(sanitized)
	if statement == "" {
		return "", invalid("请填写要执行的 SQL 语句，不能为空")
	}
	if strings.HasSuffix(statement, ";") {
		statement = strings.TrimSpace(strings.TrimSuffix(statement, ";"))
	}
	if strings.Contains(statement, ";") {
		return "", invalid("请一次只提交一条写语句，分号只能放在末尾")
	}

	keyword := firstKeyword(statement)
	if _, ok := readOnlyLeadingKeywords[keyword]; ok {
		return "", invalid("只读查询请求请走 db-postgres.query（exec 只接受写语句）")
	}
	kind := ""
	if _, ok := dataWriteKeywords[keyword]; ok {
		kind = ExecKindData
	} else if _, ok := ddlWriteKeywords[keyword]; ok {
		kind = ExecKindDDL
	} else {
		return "", invalid("写入白名单拒绝：请让语句以 INSERT/UPDATE/DELETE/MERGE 或 DROP/TRUNCATE/ALTER/CREATE/RENAME/GRANT/REVOKE/REINDEX/CLUSTER/VACUUM 开头")
	}

	upper := strings.ToUpper(statement)
	// COPY ... PROGRAM 会在数据库服务器上执行任意系统命令，是最危险的侧信道；
	// COPY 本身不在首关键字白名单里，这里点明原因，便于用户理解报错。
	if strings.Contains(upper, "COPY ") && strings.Contains(upper, " PROGRAM ") {
		return "", invalid("写入白名单拒绝：请勿用 COPY ... PROGRAM（会在数据库服务器执行系统命令）")
	}
	// 只读 sqlguard 里列出的副作用函数（文件读写/dblink/信号/WAL 操纵/序列改写）
	// 在 exec 路径同样禁止：写外壳挡不住 SELECT 前缀里藏的副作用调用。
	if match := forbiddenFunctionsRE.FindString(sanitizeSQLForFunctions(sql)); match != "" {
		name := strings.ToLower(strings.TrimSpace(strings.TrimSuffix(match, "(")))
		return "", invalid("写入白名单拒绝：请去掉有副作用或会阻塞的函数 " + name)
	}

	if !confirm {
		return "", invalid("写语句需要显式确认：请传 confirm=true")
	}
	if kind == ExecKindDDL {
		expected := confirmPhrase(statement)
		got := strings.TrimSpace(confirmText)
		if got == "" {
			return "", invalid(`危险语句需要手打确认短语：请把 confirm_text 设为 "` + expected + `"`)
		}
		if !strings.EqualFold(got, expected) {
			return "", invalid(`confirm_text 与语句不匹配，应填 "` + expected + `"`)
		}
	}
	return kind, nil
}

// confirmPhrase 取语句前两个关键字作为确认短语（大写、单空格），如 DROP TABLE / ALTER TABLE。
// PG 方言：首关键字后面可能紧跟括号或运算符（如 VACUUM (ANALYZE)），此时只返回首关键字。
func confirmPhrase(statement string) string {
	fields := strings.Fields(statement)
	words := make([]string, 0, 2)
	for _, field := range fields {
		if !isAlphaWord(field) {
			break
		}
		words = append(words, strings.ToUpper(field))
		if len(words) == 2 {
			break
		}
	}
	if len(words) == 0 {
		return ""
	}
	return strings.Join(words, " ")
}

// isAlphaWord 判断 token 是否全为 ASCII 字母（用于确认短语，标识符/括号不参与）。
func isAlphaWord(token string) bool {
	if token == "" {
		return false
	}
	for i := 0; i < len(token); i++ {
		c := token[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') {
			return false
		}
	}
	return true
}
