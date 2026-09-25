//go:build tinygo

package pluginsdk

//go:wasmimport ncgo log
func hostLog(level, ptr, length int32) int32

func Debug(msg string) { log(LevelDebug, msg) }
func Info(msg string)  { log(LevelInfo, msg) }
func Warn(msg string)  { log(LevelWarn, msg) }
func Error(msg string) { log(LevelError, msg) }

func log(level int32, msg string) {
	if msg == "" {
		_ = hostLog(level, 0, 0)
		return
	}
	ptr := allocString(msg)
	_ = hostLog(level, ptr, int32(len(msg)))
}
