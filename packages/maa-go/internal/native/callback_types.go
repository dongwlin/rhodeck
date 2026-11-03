package native

type unixCallback func(msg int32, detailsJSON *byte, customArg uintptr)

type windowsCallback func(msg int32, detailsJSON *byte, customArg uintptr) uintptr
