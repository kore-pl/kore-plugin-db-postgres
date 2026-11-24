package pgop

import (
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// OpError 是执行面带协议错误码的错误，classify 优先用它，避免错误信息二次加工时丢码。
type OpError struct {
	Code    string
	Message string
}

// Error 实现 error 接口。
func (e *OpError) Error() string { return e.Code + ": " + e.Message }

// opErrorf 构造带协议错误码的执行面错误。
func opErrorf(code, format string, args ...any) *OpError {
	return &OpError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// timeoutError 统一超时错误文案（含实际生效超时）。
func timeoutError(d time.Duration) *OpError {
	return opErrorf(pluginapi.E_UPSTREAM, "操作超时（%s），已终止 psql", d)
}

// isNilError 识别「类型化 nil」：error 接口非 nil，但底层是 nil 指针/接口/map/slice/
// func/chan。这类值调用 Error() 或解引用 errors.As 结果都会 SIGSEGV。
// 用 reflect 做通用判断，不绑死某一个错误类型——同类新类型也不会漏网。
func isNilError(err error) bool {
	if err == nil {
		return true
	}
	v := reflect.ValueOf(err)
	switch v.Kind() {
	case reflect.Ptr, reflect.Interface, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan:
		return v.IsNil()
	}
	return false
}

// errorText 把执行面错误压成可放进 per-instance error 字段的文本（不含密码）。
func errorText(err error) string {
	if isNilError(err) {
		return ""
	}
	var op *OpError
	if errors.As(err, &op) && op != nil {
		return op.Message
	}
	return err.Error()
}
