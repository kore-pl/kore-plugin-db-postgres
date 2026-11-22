package pgop

import (
	"errors"
	"regexp"
	"strings"
)

// 实例名：小写字母/数字开头，仅含小写字母、数字、点与连字符，≤64 字符。
var instanceNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,63}$`)

// 主机：允许主机名 / IPv4 / IPv6 字面量；不允许空白与 "-" 开头（避免被当成客户端选项）。
var hostRE = regexp.MustCompile(`^[A-Za-z0-9._:%-]+$`)

// 用户名：常见 PostgreSQL 账号形态；同样禁止 "-" 开头，避免 argv 选项注入。
var userRE = regexp.MustCompile(`^[A-Za-z0-9._@%-]+$`)

// 密码来源环境变量名。
var passwordEnvRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)

// 数据库名白名单：PostgreSQL 标识符里最保守的形态（ASCII 字母、数字、下划线）。
//
// 任务口径是 1..63 字节；这里刻意不接受 $ / 大写 / Unicode / 引号 / 分号，
// 因为这些字符一旦需要引号包裹就引入转义面。需要大写或特殊字符的库名不在 v1 范围。
var databaseNameRE = regexp.MustCompile(`^[a-zA-Z0-9_]{1,63}$`)

// maxPasswordBytes 与配置契约的密码上限一致，避免异常大字段进入子进程环境。
const maxPasswordBytes = 1024

// ValidateInstanceName 校验实例名（配置解析与请求参数共用同一套规则）。
func ValidateInstanceName(name string) error { return validateInstanceName(name) }

// ValidateHost 供配置契约校验默认实例主机。
func ValidateHost(host string) error { return validateHost(host) }

// ValidateUser 供配置契约校验默认实例用户名。
func ValidateUser(user string) error { return validateUser(user) }

// ValidatePasswordEnv 供配置契约校验密码环境变量名。
func ValidatePasswordEnv(name string) error {
	if name == "" {
		return errors.New("环境变量名不得为空")
	}
	if !passwordEnvRE.MatchString(name) {
		return errors.New("环境变量名非法：须以字母/下划线开头，仅含字母、数字、下划线")
	}
	return nil
}

// ValidateDatabaseName 校验数据库名标识符（^[a-zA-Z0-9_]{1,63}$）。
func ValidateDatabaseName(name string) error {
	if name == "" {
		return errors.New("数据库名不得为空")
	}
	if !databaseNameRE.MatchString(name) {
		return errors.New("数据库名非法：只允许字母、数字与下划线，长度 1-63")
	}
	return nil
}

// validateInstanceName 校验实例名，配置解析与请求参数共用。
func validateInstanceName(name string) error {
	if name == "" {
		return errors.New("实例名不得为空")
	}
	if strings.ContainsRune(name, 0) {
		return errors.New("实例名含 NUL 字节")
	}
	if strings.ContainsAny(name, `/\`) {
		return errors.New("实例名不得含路径分隔符")
	}
	if !instanceNameRE.MatchString(name) {
		return errors.New("实例名非法：须以小写字母/数字开头，仅含小写字母、数字、. 与 -，且不超过 64 字符")
	}
	if strings.Contains(name, "..") {
		return errors.New("实例名含 .. 段，禁止穿越")
	}
	return nil
}

// validateHost 校验实例主机，拒绝 argv 选项注入与不可打印字符。
func validateHost(host string) error {
	if host == "" {
		return errors.New("host 不得为空")
	}
	if len(host) > 253 {
		return errors.New("host 超过 253 字符")
	}
	if strings.HasPrefix(host, "-") {
		return errors.New("host 不得以 - 开头（防止被当作客户端选项）")
	}
	if !hostRE.MatchString(host) {
		return errors.New("host 含非法字符：仅允许字母、数字、. - _ : %")
	}
	return nil
}

// validateUser 校验用户名，拒绝 argv 选项注入。
func validateUser(user string) error {
	if user == "" {
		return errors.New("user 不得为空")
	}
	if len(user) > 128 {
		return errors.New("user 超过 128 字符")
	}
	if strings.HasPrefix(user, "-") {
		return errors.New("user 不得以 - 开头（防止被当作客户端选项）")
	}
	if !userRE.MatchString(user) {
		return errors.New("user 含非法字符：仅允许字母、数字、. - _ @ %")
	}
	return nil
}

// ValidatePassword 供配置契约校验密码：密码不进 SQL、只进环境变量，不做字符集限制。
func ValidatePassword(password string) error { return validatePassword(password) }

// validatePassword 校验密码长度与 NUL：密码不进 SQL、只进环境变量，不做字符集限制。
func validatePassword(password string) error {
	if len(password) > maxPasswordBytes {
		return errors.New("密码超过 1024 字节上限；请确认填写内容")
	}
	if strings.ContainsRune(password, 0) {
		return errors.New("密码含 NUL 字节，无法作为连接凭据")
	}
	return nil
}
