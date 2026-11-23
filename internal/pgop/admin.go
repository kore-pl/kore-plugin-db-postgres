package pgop

import (
	"encoding/json"
	"strings"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// L2 日常管理方法：建库/删库、建用户/删用户、授权、改密。
//
// 共同防线（任一缺失都不允许触达数据库）：
//  1. 写方法要求插件自身声明 system:admin（requireAdmin，纵深防御；内核已按
//     method_capabilities 授权过一次）；
//  2. 库名/用户名过白名单 + 双引号包裹（identifiers.go），口令走单引号字面量；
//  3. 删库系统库先行拒绝、删用户超级用户/保留账号先行拒绝，再要求 confirm=true；
//  4. 口令只经 stdin 到 psql，不进 argv / 日志 / 响应，错误文本脱敏。

// requireAdmin 是写方法的能力门控：manifest 未声明 system:admin 时拒绝。
func requireAdmin(cfg Config) *pluginapi.Error {
	if cfg.Admin {
		return nil
	}
	return &pluginapi.Error{
		Code:    pluginapi.E_UNAUTHORIZED,
		Message: "写操作需要 system:admin 能力：请确认 info.json 已声明该能力并经内核授予",
	}
}

// unauthorized 构造越权错误。
func unauthorized(msg string) *pluginapi.Error {
	return &pluginapi.Error{Code: pluginapi.E_UNAUTHORIZED, Message: msg}
}

// callCreateDatabase 处理 db-postgres.createDatabase。
func callCreateDatabase(raw json.RawMessage, cfg Config, exec Executor) (json.RawMessage, *pluginapi.Error) {
	if perr := requireAdmin(cfg); perr != nil {
		return nil, perr
	}
	var p createDatabaseParams
	if perr := decodeParams(raw, &p); perr != nil {
		return nil, perr
	}
	if p.Name == "" {
		return nil, invalid("缺少必填参数 name")
	}
	if err := ValidateDatabaseName(p.Name); err != nil {
		return nil, invalid(err.Error())
	}
	owner := strings.TrimSpace(p.Owner)
	if owner != "" {
		if err := ValidateRoleName(owner); err != nil {
			return nil, invalid("owner 非法: " + err.Error())
		}
	}
	instance, perr := findInstance(cfg, p.Instance)
	if perr != nil {
		return nil, perr
	}
	created, err := exec.CreateDatabase(instance, p.Name, owner)
	if err != nil {
		return nil, classify(err)
	}
	return encode(CreateDatabaseResult{Created: created, Name: p.Name, Owner: owner})
}

// callDropDatabase 处理 db-postgres.dropDatabase。
// 顺序固定：系统库保护先于 confirm，保证「删 postgres」在任何参数下都是 E_UNAUTHORIZED。
func callDropDatabase(raw json.RawMessage, cfg Config, exec Executor) (json.RawMessage, *pluginapi.Error) {
	if perr := requireAdmin(cfg); perr != nil {
		return nil, perr
	}
	var p dropDatabaseParams
	if perr := decodeParams(raw, &p); perr != nil {
		return nil, perr
	}
	if p.Name == "" {
		return nil, invalid("缺少必填参数 name")
	}
	if err := ValidateDatabaseName(p.Name); err != nil {
		return nil, invalid(err.Error())
	}
	if IsProtectedDatabase(p.Name) {
		return nil, unauthorized("数据库 " + p.Name + " 是系统库（postgres/template0/template1），拒绝删除")
	}
	if !p.Confirm {
		return nil, invalid("删库不可恢复，请传 confirm=true")
	}
	instance, perr := findInstance(cfg, p.Instance)
	if perr != nil {
		return nil, perr
	}
	dropped, err := exec.DropDatabase(instance, p.Name)
	if err != nil {
		return nil, classify(err)
	}
	return encode(DropDatabaseResult{Dropped: dropped, Name: p.Name})
}

// callCreateUser 处理 db-postgres.createUser。
func callCreateUser(raw json.RawMessage, cfg Config, exec Executor) (json.RawMessage, *pluginapi.Error) {
	if perr := requireAdmin(cfg); perr != nil {
		return nil, perr
	}
	var p createUserParams
	if perr := decodeParams(raw, &p); perr != nil {
		return nil, perr
	}
	if p.User == "" {
		return nil, invalid("缺少必填参数 user")
	}
	if err := ValidateRoleName(p.User); err != nil {
		return nil, invalid(err.Error())
	}
	if IsProtectedRoleName(p.User) {
		return nil, unauthorized("角色名 " + p.User + " 属系统保留（pg_ 前缀或 postgres），拒绝创建")
	}
	if err := validatePassword(p.Password); err != nil {
		return nil, invalid(err.Error())
	}
	instance, perr := findInstance(cfg, p.Instance)
	if perr != nil {
		return nil, perr
	}
	created, err := exec.CreateRole(instance, p.User, p.Password, p.CanCreateDb, p.CanCreateRole)
	if err != nil {
		return nil, classify(err)
	}
	return encode(CreateUserResult{Created: created, User: p.User})
}

// callDropUser 处理 db-postgres.dropUser：保留账号/confirm 先行，超级用户由执行面判定。
func callDropUser(raw json.RawMessage, cfg Config, exec Executor) (json.RawMessage, *pluginapi.Error) {
	if perr := requireAdmin(cfg); perr != nil {
		return nil, perr
	}
	var p dropUserParams
	if perr := decodeParams(raw, &p); perr != nil {
		return nil, perr
	}
	if p.User == "" {
		return nil, invalid("缺少必填参数 user")
	}
	if err := ValidateRoleName(p.User); err != nil {
		return nil, invalid(err.Error())
	}
	if IsProtectedRoleName(p.User) {
		return nil, unauthorized("角色 " + p.User + " 是系统保留/默认管理账号（pg_ 前缀或 postgres），拒绝删除")
	}
	if !p.Confirm {
		return nil, invalid("删用户不可恢复，请传 confirm=true")
	}
	instance, perr := findInstance(cfg, p.Instance)
	if perr != nil {
		return nil, perr
	}
	dropped, err := exec.DropRole(instance, p.User)
	if err != nil {
		return nil, classify(err)
	}
	return encode(DropUserResult{Dropped: dropped, User: p.User})
}

// callGrant 处理 db-postgres.grant：库级权限白名单归一化后授权。
func callGrant(raw json.RawMessage, cfg Config, exec Executor) (json.RawMessage, *pluginapi.Error) {
	if perr := requireAdmin(cfg); perr != nil {
		return nil, perr
	}
	var p grantParams
	if perr := decodeParams(raw, &p); perr != nil {
		return nil, perr
	}
	if p.User == "" {
		return nil, invalid("缺少必填参数 user")
	}
	if p.Database == "" {
		return nil, invalid("缺少必填参数 database")
	}
	if err := ValidateRoleName(p.User); err != nil {
		return nil, invalid(err.Error())
	}
	if err := ValidateDatabaseName(p.Database); err != nil {
		return nil, invalid(err.Error())
	}
	privileges, perr := normalizePrivileges(p.Privileges)
	if perr != nil {
		return nil, perr
	}
	instance, perr := findInstance(cfg, p.Instance)
	if perr != nil {
		return nil, perr
	}
	if err := exec.Grant(instance, p.User, p.Database, privileges, p.WithGrantOption); err != nil {
		return nil, classify(err)
	}
	return encode(GrantResult{
		Granted: true, User: p.User, Database: p.Database,
		Privileges: privileges, WithGrantOption: p.WithGrantOption,
	})
}

// callChangePassword 处理 db-postgres.changePassword：口令只经 stdin，回包不含口令。
func callChangePassword(raw json.RawMessage, cfg Config, exec Executor) (json.RawMessage, *pluginapi.Error) {
	if perr := requireAdmin(cfg); perr != nil {
		return nil, perr
	}
	var p changePasswordParams
	if perr := decodeParams(raw, &p); perr != nil {
		return nil, perr
	}
	if p.User == "" {
		return nil, invalid("缺少必填参数 user")
	}
	if strings.TrimSpace(p.Password) == "" {
		return nil, invalid("缺少必填参数 password")
	}
	if err := ValidateRoleName(p.User); err != nil {
		return nil, invalid(err.Error())
	}
	if IsProtectedRoleName(p.User) {
		return nil, unauthorized("角色 " + p.User + " 是系统保留/默认管理账号（pg_ 前缀或 postgres），拒绝改密")
	}
	if err := validatePassword(p.Password); err != nil {
		return nil, invalid(err.Error())
	}
	instance, perr := findInstance(cfg, p.Instance)
	if perr != nil {
		return nil, perr
	}
	if err := exec.ChangePassword(instance, p.User, p.Password); err != nil {
		return nil, classify(err)
	}
	return encode(ChangePasswordResult{Changed: true, User: p.User})
}
