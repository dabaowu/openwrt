package capwap

// 该库被tms-server和tms-apr程序引用
// tms-server使用beelogger作为日志库,
// tms-apr使用syslog作为日志库,
// 定义logger库接口, 模式使用beelogger,
// tms-apr在引用此库时通过公共设置, 使用syslog库

type Logger interface {
	Debug(fmt string, args ...interface{})
	Info(fmt string, args ...interface{})
	Notice(fmt string, args ...interface{})
	Warning(fmt string, args ...interface{})
	Error(fmt string, args ...interface{})
}

var logger Logger

func SetLogger(l Logger) {
	logger = l
}
