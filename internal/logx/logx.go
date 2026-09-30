package logx

// logx.go —— 全项目统一的极简日志（控制台 + 文件滚动 + 给数据库的 runlog 缓冲）
//
// 分层说明：这是「基础设施层」，位于 internal/logx。
// 它刻意不 import internal/conf —— 否则 conf（读配置时要打日志）和 logx（写文件
// 要知道日志目录）就互相依赖了。解耦办法：logx 只认一个 4 方法的小接口 Sink，
// 由 conf.Config 实现，并在配置加载完成后通过 SetSink 注入。
//
// 日志文件固定落在配置里的 store.log_dir（默认 runtime/logs/），不写项目其它目录。

import (
	"finally-main/internal/model"

	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"sync"
	"time"
)

// Sink 日志落盘需要的配置视图。由 conf.Config 实现。
type Sink interface {
	LogEnabled() bool       // store.enabled
	LogDirResolved() string // store.log_dir（已解析成绝对路径）
	LogMaxMBytes() int      // store.log_max_mb
	LogKeepFiles() int      // store.log_keep
}

var (
	logMu       sync.Mutex
	logFilePath string
	logDirUsed  string

	// sink 由 conf.LoadConfig 注入。logx 绝不能回头调用 LoadConfig ——
	// LoadConfig 自己会打日志，而 sync.Mutex 不可重入，那样会直接死锁。
	sinkMu sync.Mutex
	sink   Sink

	rlMu  sync.Mutex
	rlBuf []model.RunLogRow
	rlMax = 4000
)

// SetSink 装载配置视图（由 conf.LoadConfig 调用）
func SetSink(s Sink) {
	sinkMu.Lock()
	sink = s
	sinkMu.Unlock()
}

func currentSink() Sink {
	sinkMu.Lock()
	defer sinkMu.Unlock()
	return sink
}

// Logf 写一行日志：控制台 + 文件 + runlog 缓冲
func Logf(level, format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	line := time.Now().Format("2006-01-02 15:04:05.000") + " [" + level + "] " + msg
	fmt.Println(line)
	appendRunLog(level, msg)
	writeLogFile(line)
}

// DebugStack 当前调用栈（panic 恢复时打日志用）
func DebugStack() string { return string(debug.Stack()) }

func writeLogFile(line string) {
	s := currentSink()
	if s == nil || !s.LogEnabled() {
		return
	}
	dir := s.LogDirResolved()
	if dir == "" {
		return
	}

	logMu.Lock()
	defer logMu.Unlock()

	if logDirUsed != dir {
		logDirUsed = dir
		logFilePath = ""
	}
	if logFilePath == "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return
		}
		logFilePath = filepath.Join(dir, "okxbot.log")
	}

	if fi, err := os.Stat(logFilePath); err == nil {
		if fi.Size() > int64(s.LogMaxMBytes())*1024*1024 {
			rotateLog(logFilePath, s.LogKeepFiles())
		}
	}
	f, err := os.OpenFile(logFilePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	f.WriteString(line + "\n")
	f.Close()
}

func rotateLog(path string, keep int) {
	if keep < 1 {
		keep = 1
	}
	for i := keep - 1; i >= 1; i-- {
		old := path + "." + strconv.Itoa(i)
		if _, err := os.Stat(old); err == nil {
			os.Remove(path + "." + strconv.Itoa(i+1))
			os.Rename(old, path+"."+strconv.Itoa(i+1))
		}
	}
	os.Remove(path + ".1")
	os.Rename(path, path+".1")
}

func appendRunLog(level, msg string) {
	rlMu.Lock()
	defer rlMu.Unlock()
	if len(rlBuf) >= rlMax {
		rlBuf = rlBuf[len(rlBuf)/2:]
	}
	rlBuf = append(rlBuf, model.RunLogRow{Ts: time.Now().UnixMilli(), Level: level, Msg: msg})
}

// TakeRunLogs 取走缓冲（写库用）
func TakeRunLogs() []model.RunLogRow {
	rlMu.Lock()
	defer rlMu.Unlock()
	if len(rlBuf) == 0 {
		return nil
	}
	out := rlBuf
	rlBuf = nil
	return out
}
