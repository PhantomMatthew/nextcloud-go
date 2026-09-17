package pluginsdk

// ABIVersion is the ncgo-abi/1 numeric version exported by plugins.
const ABIVersion int32 = 1

const (
	LevelDebug int32 = 0
	LevelInfo  int32 = 1
	LevelWarn  int32 = 2
	LevelError int32 = 3
)

const (
	ErrCodeOK               int32 = 0
	ErrCodeInternal         int32 = -1
	ErrCodeInvalidArgument  int32 = -2
	ErrCodePermissionDenied int32 = -3
	ErrCodeNotFound         int32 = -4
	ErrCodeAlreadyExists    int32 = -5
	ErrCodeTimeout          int32 = -6
	ErrCodeCanceled         int32 = -7
	ErrCodeQuotaExceeded    int32 = -8
	ErrCodeUnsupported      int32 = -9
	ErrCodeConflict         int32 = -10
	ErrCodeTooLarge         int32 = -11
	ErrCodeUnavailable      int32 = -12
)
