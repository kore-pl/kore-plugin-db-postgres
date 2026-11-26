package pgop

import (
	"errors"
	"regexp"
	"strings"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// 本文件是写操作的标识符与字面量防线：白名单校验 + 引号包裹 + 字面量转义。
//
// 两道防线同时存在（不是二选一）：
//  1. 白名单：标识符只允许 [a-zA-Z0-9_]{1,63}，从源头上没有引号/分号/反斜杠；
//  2. 引号：写进 SQL 时仍用 PostgreSQL 的 "..." 标识符引用并转义内部 "，
//     这样即使将来放宽白名单，也不会因为一个双引号跳出标识符上下文。
//
// 绝不把用户输入直接拼进 SQL——所有拼接点都在本文件的 quote* 函数之后。

const (
	// maxIdentifierBytes 是 PostgreSQL 标识符的字节上限（NAMEDATALEN-1）。
	maxIdentifierBytes = 63
	// reservedRolePrefix 是 PostgreSQL 保留给系统角色的前缀，任何建/删/改密都拒绝。
	reservedRolePrefix = "pg_"
	// defaultAdminRole 是 PostgreSQL 的默认管理账号名，禁止删除/改密（防自锁）。
	defaultAdminRole = "postgres"
	// maxGrantPrivileges 限制单次 GRANT 的权限条数（白名单只有 4 项，留余量）。
	maxGrantPrivileges = 8
)

// roleNameRE 与 databaseNameRE 同级保守：ASCII 字母、数字、下划线。
//
// 大小写保留（带引号的标识符区分大小写），需要空格/连字符/Unicode 的名字不在 v1 范围。
var roleNameRE = regexp.MustCompile(`^[a-zA-Z0-9_]{1,63}$`)

// tablePatternRE 是表级备份允许的表名形状：table 或 schema.table。
//
// 不放行 pg_dump 的 % / * 通配符：备份范围必须是用户明确点出的对象，
// 通配符会把「备份一张表」变成「备份一个 schema」，属于用户没同意的范围扩大。
var tablePatternRE = regexp.MustCompile(`^[a-zA-Z0-9_]{1,63}(\.[a-zA-Z0-9_]{1,63})?$`)

// protectedDatabases 是禁止 DROP 的系统库（比较时忽略大小写，防止 "Postgres" 绕过）。
var protectedDatabases = map[string]struct{}{
	"postgres":  {},
	"template0": {},
	"template1": {},
}

// databaseGrantPrivileges 是库级 GRANT 允许的权限白名单（大写为归一化目标）。
//
// 只放行 CONNECT / CREATE / TEMPORARY / ALL：这是「让别人能连/能建 schema」的最小集合。
// 明确不接受 ALL PRIVILEGES、GRANT OPTION 之外的写法，也不做 schema/table 级授权
// ——表级授权需要 REVOKE 与所有权模型，v1 不开这个口子。
var databaseGrantPrivileges = map[string]string{
	"CONNECT":   "CONNECT",
	"CREATE":    "CREATE",
	"TEMP":      "TEMPORARY",
	"TEMPORARY": "TEMPORARY",
	"ALL":       "ALL",
}

// grantPrivilegeOrder 只用于错误提示里的可读列表。
const grantPrivilegeOrder = "CONNECT/CREATE/TEMP(TEMPORARY)/ALL"

// ValidateRoleName 校验角色（用户）名：ASCII 字母、数字、下划线，1..63 字节。
func ValidateRoleName(name string) error {
	if name == "" {
		return errors.New("用户名不得为空")
	}
	if len(name) > maxIdentifierBytes {
		return errors.New("用户名超过 63 字节")
	}
	if !roleNameRE.MatchString(name) {
		return errors.New("用户名非法：只允许字母、数字与下划线，长度 1-63")
	}
	return nil
}

// validateTablePattern 校验表级备份的单个表名（table 或 schema.table）。
func validateTablePattern(name string) error {
	if name == "" {
		return errors.New("表名不得为空")
	}
	if !tablePatternRE.MatchString(name) {
		return errors.New("表名 " + name + " 非法：只允许「表名」或「schema.表名」，各段只含字母、数字与下划线，长度 1-63")
	}
	return nil
}

// IsProtectedRoleName 判断角色名是否落在硬保护集合：pg_ 前缀（系统保留）与 postgres。
// 比较忽略大小写：PostgreSQL 对未加引号的名字折小写，这里不让大小写变体绕过。
func IsProtectedRoleName(name string) bool {
	lower := strings.ToLower(strings.TrimSpace(name))
	if lower == defaultAdminRole {
		return true
	}
	return strings.HasPrefix(lower, reservedRolePrefix)
}

// IsProtectedDatabase 判断库名是否为系统库（postgres / template0 / template1）。
func IsProtectedDatabase(name string) bool {
	_, protected := protectedDatabases[strings.ToLower(strings.TrimSpace(name))]
	return protected
}

// quoteIdentifier 把已过白名单的标识符包成 PostgreSQL 的 "..." 形式。
// 内部双引号按 PostgreSQL 规则双写；白名单本不允许引号，这里是第二道防线。
func quoteIdentifier(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// quoteLiteral 把字符串包成 PostgreSQL 单引号字面量。
//
// standard_conforming_strings=on 时普通字符串里只有单引号需要转义（双写），
// 反斜杠是普通字符——因此这里只双写单引号，不碰反斜杠（碰了就会改掉口令原文）。
func quoteLiteral(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

// normalizePrivileges 把入参权限归一化为大写白名单值并去重（保持入参顺序）。
func normalizePrivileges(privileges []string) ([]string, *pluginapi.Error) {
	if len(privileges) == 0 {
		return nil, invalid("请填写必填参数 privileges（权限，可选值: " + grantPrivilegeOrder + "）")
	}
	if len(privileges) > maxGrantPrivileges {
		return nil, invalid("权限 privileges 最多填 8 项，请减少后再试")
	}
	normalized := make([]string, 0, len(privileges))
	seen := make(map[string]struct{}, len(privileges))
	for _, raw := range privileges {
		key := strings.ToUpper(strings.TrimSpace(raw))
		canonical, ok := databaseGrantPrivileges[key]
		if !ok {
			return nil, invalid("请检查填写的权限 " + raw + " 不在白名单内（可选值: " + grantPrivilegeOrder + "）")
		}
		if _, dup := seen[canonical]; dup {
			continue
		}
		seen[canonical] = struct{}{}
		normalized = append(normalized, canonical)
	}
	return normalized, nil
}
