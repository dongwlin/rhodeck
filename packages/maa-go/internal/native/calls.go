package native

import "unsafe"

func (t *APITable) GetVersion() *byte { return t.api.GetVersion() }

func (t *APITable) GetNullSize() uint64 { return t.api.GetNullSize() }

func (t *APITable) SetUserDir(path *byte) uint8 { return t.api.SetUserDir(path) }

func (t *APITable) SetStaticOption(key int32, value *byte) uint8 {
	return t.api.SetStaticOption(key, value)
}

func (t *APITable) LoadResource(path *byte) uint8 { return t.api.LoadResource(path) }

func (t *APITable) SetConnectionExtras(name *byte, extras *byte) {
	t.api.SetConnectionExtras(name, extras)
}

func (t *APITable) Stop(handle uintptr) uint8 { return t.api.Stop(handle) }

func (t *APITable) Destroy(handle uintptr) { t.api.Destroy(handle) }

func (t *APITable) CreateEx(callback uintptr, customArg uintptr) uintptr {
	return t.api.CreateEx(callback, customArg)
}

func (t *APITable) SetInstanceOption(handle uintptr, key int32, value *byte) uint8 {
	return t.api.SetInstanceOption(handle, key, value)
}

func (t *APITable) AsyncConnect(handle uintptr, adbPath *byte, address *byte, config *byte, block uint8) int32 {
	return t.api.AsyncConnect(handle, adbPath, address, config, block)
}

func (t *APITable) AsyncClick(handle uintptr, x int32, y int32, block uint8) int32 {
	return t.api.AsyncClick(handle, x, y, block)
}

func (t *APITable) AsyncScreencap(handle uintptr, block uint8) int32 {
	return t.api.AsyncScreencap(handle, block)
}

func (t *APITable) AppendTask(handle uintptr, taskType *byte, params *byte) int32 {
	return t.api.AppendTask(handle, taskType, params)
}

func (t *APITable) SetTaskParams(handle uintptr, id int32, params *byte) uint8 {
	return t.api.SetTaskParams(handle, id, params)
}

func (t *APITable) Start(handle uintptr) uint8 { return t.api.Start(handle) }

func (t *APITable) Running(handle uintptr) uint8 { return t.api.Running(handle) }

func (t *APITable) Connected(handle uintptr) uint8 { return t.api.Connected(handle) }

func (t *APITable) BackToHome(handle uintptr) uint8 { return t.api.BackToHome(handle) }

func (t *APITable) Log(level *byte, message *byte) { t.api.Log(level, message) }

func (t *APITable) GetImage(handle uintptr, buff unsafe.Pointer, buffSize uint64) uint64 {
	return t.api.GetImage(handle, buff, buffSize)
}

func (t *APITable) GetImageBgr(handle uintptr, buff unsafe.Pointer, buffSize uint64) uint64 {
	return t.api.GetImageBgr(handle, buff, buffSize)
}

func (t *APITable) AsyncAttachWindow(handle uintptr, hwnd uintptr, screencapMethod uint64, mouseMethod uint64, keyboardMethod uint64, block uint8) int32 {
	return t.api.AsyncAttachWindow(handle, hwnd, screencapMethod, mouseMethod, keyboardMethod, block)
}

func (t *APITable) GetMapLevelKey(key *byte) mapLevelKey { return t.api.GetMapLevelKey(key) }

func (t *APITable) GetItemName(id *byte) *byte { return t.api.GetItemName(id) }

func (t *APITable) GetUUID(handle uintptr, buff *byte, buffSize uint64) uint64 {
	return t.api.GetUUID(handle, buff, buffSize)
}

func (t *APITable) GetTasksList(handle uintptr, buff *int32, buffSize uint64) uint64 {
	return t.api.GetTasksList(handle, buff, buffSize)
}
