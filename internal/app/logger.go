package app

import (
	"log/slog"
	"os"

	"github.com/ymedword/basalt/internal/cli"
)

// NewLogger 按配置装配结构化日志器：dev 文本档 / prod JSON 档。
// NewLogger wires the structured logger: a text profile for dev and a JSON
// profile for prod.
//
// cfg 为 nil 表示"配置尚未装载"——此时取缺省档（文本 / info）。引导期必须先有
// 日志出口，否则配置装载本身失败就无迹可寻。
// A nil cfg means "configuration not loaded yet" and selects the default
// profile: the bootstrap path needs a logger precisely when loading fails.
//
// 日志一律写标准错误：标准输出留给命令结果本身，两路分离才能让状态报告
// 与日志各自可读、各自可被测试独立捕获。
// Logs go to stderr so that stdout carries command results only.
func NewLogger(cfg *cli.Config) *slog.Logger {
	level := slog.LevelInfo
	format := "text"
	if cfg != nil {
		level = logLevel(cfg.Log.Level)
		if cfg.Log.Format != "" {
			format = cfg.Log.Format
		}
	}

	opts := &slog.HandlerOptions{Level: level}
	var handler slog.Handler
	if format == "json" {
		handler = slog.NewJSONHandler(os.Stderr, opts)
	} else {
		handler = slog.NewTextHandler(os.Stderr, opts)
	}
	return slog.New(handler)
}

// logLevel 把配置取值翻译成 slog 级别——取值域已由 cli.LoadConfig 判卷，
// 这里只做翻译（缺省 info）。
// logLevel translates a validated configuration value into a slog level.
func logLevel(level string) slog.Level {
	switch level {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
