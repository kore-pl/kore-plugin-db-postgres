package pgop

import (
	"strconv"
	"strings"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// 本文件是 L2 写操作的真实执行面：建库/删库、建用户/删用户/授权/改密。
//
// 通道约定：所有 DDL/DCL 走 runStdin（SQL 从 stdin 读），口令只以字面量出现在
// stdin 流里——不进 argv（本机 ps 可见）、不进 docker exec 的命令行、不进日志。
// 只读的存在性检查走 run（-c），这些 SQL 不含任何口令。
//
// 存在性检查的意义在于把「已存在 / 不存在」变成结构化语义（created=false /
// E_NOTFOUND），而不是把 PostgreSQL 的原始报错甩给用户。

// scalarInt 执行单值计数查询并解析为 int64。
func (c *Client) scalarInt(instance InstanceConfig, sql string) (int64, error) {
	row, perr := c.firstRow(instance, sql)
	if perr != nil {
		return 0, perr
	}
	value, err := strconv.ParseInt(strings.TrimSpace(row[0]), 10, 64)
	if err != nil {
		return 0, opErrorf(pluginapi.E_UPSTREAM, "计数查询结果不是整数: %q", row[0])
	}
	return value, nil
}

// firstRow 执行单行查询并返回第一行单元格（空结果集归 E_UPSTREAM）。
func (c *Client) firstRow(instance InstanceConfig, sql string) ([]string, *OpError) {
	stdout, perr := c.run(instance, sql, c.connectTimeout())
	if perr != nil {
		return nil, perr
	}
	_, rows, _, err := parseCSV(stdout, 1, maxCellBytes)
	if err != nil {
		return nil, opErrorf(pluginapi.E_INTERNAL, "解析查询结果失败: %v", err)
	}
	if len(rows) != 1 || len(rows[0]) == 0 {
		return nil, opErrorf(pluginapi.E_UPSTREAM, "期望 1 行查询结果，实际 %d 行", len(rows))
	}
	return rows[0], nil
}

// databaseExistsSQL 构造库存在性检查（name 已过白名单，仍按字面量转义）。
func databaseExistsSQL(name string) string {
	return "SELECT count(*) FROM pg_database WHERE datname = " + quoteLiteral(name)
}

// roleExistsSQL 构造角色存在性检查。
func roleExistsSQL(user string) string {
	return "SELECT count(*) FROM pg_roles WHERE rolname = " + quoteLiteral(user)
}

// roleFlagsSQL 查询角色的超级用户标记与「是不是当前连接角色」。
func roleFlagsSQL(user string) string {
	return "SELECT rolsuper::text, (rolname = current_user)::text FROM pg_roles WHERE rolname = " +
		quoteLiteral(user)
}

// execDDL 执行一条 DDL/DCL（SQL 只走 stdin）。
func (c *Client) execDDL(instance InstanceConfig, sql string) *OpError {
	_, perr := c.runStdin(instance, sql, c.queryTimeout())
	return perr
}

// CreateDatabase 建库；库已存在时返回 created=false 且不报错。
func (c *Client) CreateDatabase(instance InstanceConfig, name, owner string) (bool, error) {
	if err := ValidateDatabaseName(name); err != nil {
		return false, opErrorf(pluginapi.E_INVALID, "%s", err.Error())
	}
	if owner != "" {
		if err := ValidateRoleName(owner); err != nil {
			return false, opErrorf(pluginapi.E_INVALID, "owner 非法: %s", err.Error())
		}
	}
	count, err := c.scalarInt(instance, databaseExistsSQL(name))
	if err != nil {
		return false, err
	}
	if count > 0 {
		return false, nil
	}
	if perr := c.execDDL(instance, createDatabaseSQL(name, owner)); perr != nil {
		return false, perr
	}
	return true, nil
}

// DropDatabase 删库；库不存在返回 E_NOTFOUND，而不是静默成功。
func (c *Client) DropDatabase(instance InstanceConfig, name string) (bool, error) {
	if err := ValidateDatabaseName(name); err != nil {
		return false, opErrorf(pluginapi.E_INVALID, "%s", err.Error())
	}
	count, err := c.scalarInt(instance, databaseExistsSQL(name))
	if err != nil {
		return false, err
	}
	if count == 0 {
		return false, opErrorf(pluginapi.E_NOTFOUND, "数据库 %q 不存在", name)
	}
	if perr := c.execDDL(instance, dropDatabaseSQL(name)); perr != nil {
		return false, perr
	}
	return true, nil
}

// CreateRole 建角色；已存在时 created=false 且不修改口令（改密是单独方法）。
func (c *Client) CreateRole(instance InstanceConfig, user, password string, canCreateDB, canCreateRole bool) (bool, error) {
	if err := ValidateRoleName(user); err != nil {
		return false, opErrorf(pluginapi.E_INVALID, "%s", err.Error())
	}
	if err := validatePassword(password); err != nil {
		return false, opErrorf(pluginapi.E_INVALID, "%s", err.Error())
	}
	count, err := c.scalarInt(instance, roleExistsSQL(user))
	if err != nil {
		return false, err
	}
	if count > 0 {
		return false, nil
	}
	if perr := c.execDDL(instance, createRoleSQL(user, password, canCreateDB, canCreateRole)); perr != nil {
		return false, perr
	}
	return true, nil
}

// DropRole 删角色：不存在 → E_NOTFOUND；超级用户 / 当前连接角色 → E_UNAUTHORIZED。
//
// 超级用户保护在执行面做（而不是只靠名字）：rolsuper 是数据库自己的事实，
// 改名或大小写变体都绕不过它。
func (c *Client) DropRole(instance InstanceConfig, user string) (bool, error) {
	if err := ValidateRoleName(user); err != nil {
		return false, opErrorf(pluginapi.E_INVALID, "%s", err.Error())
	}
	if IsProtectedRoleName(user) {
		return false, opErrorf(pluginapi.E_UNAUTHORIZED,
			"角色 %q 是系统保留/默认管理账号（pg_ 前缀或 postgres），拒绝删除", user)
	}
	row, perr := c.firstRow(instance, roleFlagsSQL(user))
	if perr != nil {
		return false, perr
	}
	if len(row) < 2 {
		return false, opErrorf(pluginapi.E_UPSTREAM, "角色属性查询返回列数不足")
	}
	if strings.TrimSpace(row[0]) == "t" {
		return false, opErrorf(pluginapi.E_UNAUTHORIZED,
			"角色 %q 是超级用户，拒绝删除（先降权再删）", user)
	}
	if strings.TrimSpace(row[1]) == "t" {
		return false, opErrorf(pluginapi.E_UNAUTHORIZED,
			"角色 %q 是当前连接使用的账号，拒绝删除", user)
	}
	if perr := c.execDDL(instance, dropRoleSQL(user)); perr != nil {
		return false, perr
	}
	return true, nil
}

// Grant 授予库级权限：角色与库都必须存在，权限已过白名单。
func (c *Client) Grant(instance InstanceConfig, user, database string, privileges []string, withGrantOption bool) error {
	if err := ValidateRoleName(user); err != nil {
		return opErrorf(pluginapi.E_INVALID, "%s", err.Error())
	}
	if err := ValidateDatabaseName(database); err != nil {
		return opErrorf(pluginapi.E_INVALID, "%s", err.Error())
	}
	count, err := c.scalarInt(instance, roleExistsSQL(user))
	if err != nil {
		return err
	}
	if count == 0 {
		return opErrorf(pluginapi.E_NOTFOUND, "角色 %q 不存在", user)
	}
	count, err = c.scalarInt(instance, databaseExistsSQL(database))
	if err != nil {
		return err
	}
	if count == 0 {
		return opErrorf(pluginapi.E_NOTFOUND, "数据库 %q 不存在", database)
	}
	// 成功路径必须返回裸 nil：直接 return *OpError 会把类型化 nil 装进 error 接口，
	// 调用方 err != nil 为真，classify 的 errors.As 命中后解引用 nil 会 SIGSEGV。
	if perr := c.execDDL(instance, grantSQL(user, database, privileges, withGrantOption)); perr != nil {
		return perr
	}
	return nil
}

// ChangePassword 改密：ALTER ROLE ... WITH PASSWORD，口令只出现在 stdin 流里。
func (c *Client) ChangePassword(instance InstanceConfig, user, password string) error {
	if err := ValidateRoleName(user); err != nil {
		return opErrorf(pluginapi.E_INVALID, "%s", err.Error())
	}
	if strings.TrimSpace(password) == "" {
		return opErrorf(pluginapi.E_INVALID, "缺少必填参数 password")
	}
	if err := validatePassword(password); err != nil {
		return opErrorf(pluginapi.E_INVALID, "%s", err.Error())
	}
	count, err := c.scalarInt(instance, roleExistsSQL(user))
	if err != nil {
		return err
	}
	if count == 0 {
		return opErrorf(pluginapi.E_NOTFOUND, "角色 %q 不存在", user)
	}
	// 与 Grant 同理：成功返回裸 nil，杜绝 (*OpError)(nil) 装箱成非 nil error。
	if perr := c.execDDL(instance, alterPasswordSQL(user, password)); perr != nil {
		return perr
	}
	return nil
}
