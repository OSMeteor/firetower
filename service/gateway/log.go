package gateway

import (
	"fmt"
	"io"
	"os"
	"time"

	"github.com/OSMeteor/firetower/socket"
)

var (
	// LogLevel Log Level 支持三种模式
	// INFO 打印所有日志信息
	// WARN 只打印警告及错误类型的日志信息
	// ERROR 只打印错误日志
	LogLevel = "INFO"
	// DefaultWriter 正常日志的默认写入方式
	DefaultWriter io.Writer = os.Stdout
	// DefaultErrorWriter 错误日志的默认写入方式
	DefaultErrorWriter io.Writer = os.Stderr
)

// 打印日志信息
// 输出格式 [firetower] 2018-08-09 17:49:30 INFO info
func logInfo(t *FireTower, info string) {
	callTowerLogger(t, "INFO", info)
}

func towerLog(t *FireTower, types, err string) {
	switch types {
	case "INFO":
		if LogLevel != "INFO" {
			return
		}
	case "WARN":
		if LogLevel == "ERROR" {
			return
		}
	}
	var (
		connID   uint64
		clientID string
		userID   string
		started  time.Time
	)
	if t != nil {
		connID = t.connId
		clientID = t.ClientId
		userID = t.UserId
		started = t.startTime
	}
	writer := DefaultErrorWriter
	if writer == nil {
		writer = os.Stderr
	}
	fmt.Fprintf(
		writer,
		"[FireTower] %s %s %s | LOGTIME %s | RUNTIME %s %v %s | CONNID %d | CLIENTID %s | USERID %s | LOG %s\n",
		socket.Green, types, socket.Reset,
		time.Now().Format("2006-01-02 15:04:05"),
		socket.Green, started.Format("2006-01-02 15:04:05"), socket.Reset,
		connID,
		clientID,
		userID,
		err)
}

func fireLog(f *FireInfo, types, info string) {
	if f == nil || f.Context == nil || f.Message == nil {
		return
	}
	out := DefaultWriter
	errw := DefaultErrorWriter
	if out == nil {
		out = os.Stderr
	}
	if errw == nil {
		errw = os.Stderr
	}
	if types == "INFO" {
		if LogLevel != "INFO" {
			return
		}
		fmt.Fprintf(
			out,
			"[FireInfo] %s %s %s | LOGTIME %s | RUNTIME %s %v %s | MSGID %s | EVENT %s %s %s | TOPIC %s %s %s | DATA %s | LOG %s\n",
			socket.Green, types, socket.Reset,
			time.Now().Format("2006-01-02 15:04:05"),
			socket.Green, time.Since(f.Context.startTime), socket.Reset,
			f.Context.id,
			socket.Yellow, f.Message.Type, socket.Reset,
			socket.Cyan, f.Message.Topic, socket.Reset,
			string(f.Message.Data),
			info)
	} else {
		if f != nil && f.Context != nil {
			fmt.Fprintf(
				errw,
				"[FireInfo] %s %s %s | LOGTIME %s | RUNTIME %s %v %s | MSGID %s | EVENT %s %s %s | TOPIC %s %s %s | DATA %s | LOG %s\n",
				socket.Red, types, socket.Reset,
				time.Now().Format("2006-01-02 15:04:05"),
				socket.Green, time.Since(f.Context.startTime), socket.Reset,
				f.Context.id,
				socket.Yellow, f.Message.Type, socket.Reset,
				socket.Cyan, f.Message.Topic, socket.Reset,
				string(f.Message.Data),
				info)
		}

	}

}
