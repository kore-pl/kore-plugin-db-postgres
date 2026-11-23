package pgop

import "strings"

// 本文件是写操作的 SQL 构造：全部为纯函数，便于单测断言
// 「标识符被引号包裹、权限来自白名单、口令只出现在字面量里」。
//
// 所有标识符在进入这里之前已过白名单；这里仍然统一走 quoteIdentifier / quoteLiteral，
// 保证拼接点只有一处、可被测试逐条盯住。

// createDatabaseSQL 构造建库语句；owner 非空时带 OWNER 子句。
//
// CREATE DATABASE 不能在事务块里执行，psql 单语句 stdin 批处理天然满足。
func createDatabaseSQL(name, owner string) string {
	sql := "CREATE DATABASE " + quoteIdentifier(name)
	if owner != "" {
		sql += " OWNER " + quoteIdentifier(owner)
	}
	return sql + ";\n"
}

// dropDatabaseSQL 构造删库语句（不带 FORCE：先让「有连接」成为明确的上游错误，
// 而不是悄悄踢掉别人的会话）。
func dropDatabaseSQL(name string) string {
	return "DROP DATABASE " + quoteIdentifier(name) + ";\n"
}

// createRoleSQL 构造建角色语句：LOGIN 必带，口令可选，CREATEDB/CREATEROLE 显式声明。
//
// 口令只以字面量出现在返回的 SQL 里，而这条 SQL 只经 stdin 传递（见 runStdin），
// 不进 argv / ps / 日志。
func createRoleSQL(user, password string, canCreateDB, canCreateRole bool) string {
	parts := []string{"CREATE ROLE " + quoteIdentifier(user), "LOGIN"}
	if canCreateDB {
		parts = append(parts, "CREATEDB")
	}
	if canCreateRole {
		parts = append(parts, "CREATEROLE")
	}
	if password != "" {
		parts = append(parts, "PASSWORD "+quoteLiteral(password))
	}
	return strings.Join(parts, " ") + ";\n"
}

// dropRoleSQL 构造删角色语句（DROP ROLE 对「仍拥有对象」的账号会失败，保留该保护）。
func dropRoleSQL(user string) string {
	return "DROP ROLE " + quoteIdentifier(user) + ";\n"
}

// grantSQL 构造库级授权语句；privileges 必须已白名单归一化。
func grantSQL(user, database string, privileges []string, withGrantOption bool) string {
	sql := "GRANT " + strings.Join(privileges, ", ") + " ON DATABASE " + quoteIdentifier(database) +
		" TO " + quoteIdentifier(user)
	if withGrantOption {
		sql += " WITH GRANT OPTION"
	}
	return sql + ";\n"
}

// alterPasswordSQL 构造改密语句：ALTER ROLE ... WITH PASSWORD 是 PostgreSQL 唯一
// 由 SQL 驱动的改密方式（\password 是 psql 交互元命令，不适合子进程通道）。
func alterPasswordSQL(user, password string) string {
	return "ALTER ROLE " + quoteIdentifier(user) + " WITH PASSWORD " + quoteLiteral(password) + ";\n"
}
