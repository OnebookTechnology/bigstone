package LevelLogger_go

import (
	"fmt"
	"log"
	"os"
)

const (
	LogError = iota
	LogInfo
	LogWarning
	LogDebug
)

var (
	LevelStringMap = map[int]string{
		LogError:   "ERROR",
		LogInfo:    "INFO",
		LogWarning: "WARNING",
		LogDebug:   "DEBUG",
	}
)

type LevelLogger struct {
	logger   *log.Logger
	logLevel int
}

func NewLevelLogger(logFile *os.File, prefix string, flag int, level int) (*LevelLogger, error) {
	_, err := logFile.Stat()
	if err != nil {
		return nil, err
	}
	return &LevelLogger{log.New(logFile, prefix, flag), level}, nil
}

func (l *LevelLogger) Error(v ...interface{}) {
	if l.logLevel >= LogError {
		l.PrintLevelLog(LogError, v...)
	}
}

func (l *LevelLogger) Warning(v ...interface{}) {
	if l.logLevel >= LogWarning {
		l.PrintLevelLog(LogWarning, v...)
	}
}

func (l *LevelLogger) Info(v ...interface{}) {
	if l.logLevel >= LogInfo {
		l.PrintLevelLog(LogInfo, v...)
	}
}

func (l *LevelLogger) Debug(v ...interface{}) {
	if l.logLevel >= LogDebug {
		l.PrintLevelLog(LogDebug, v...)
	}
}

func (l *LevelLogger) PrintLevelLog(level int, v ...interface{}) {
	printLevelLog(l.logger, level, v...)
}

func printLevelLog(logger *log.Logger, level int, v ...interface{}) {
	f := logFormat(LevelStringMap[level], v...)
	logger.Printf(f, v...)
}

func logFormat(level string, v ...interface{}) string {
	formatStr := fmt.Sprintf("%-8s", "[" +level + "]")
	for i := 0; i < len(v); i++ {
		formatStr += "%v "
	}
	formatStr += "\n"
	return formatStr
}
