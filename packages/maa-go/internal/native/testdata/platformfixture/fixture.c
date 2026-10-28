#include <stdint.h>

#ifdef _WIN32
#define EXPORT __declspec(dllexport)
#define CALLBACK_CALL __stdcall
__declspec(dllimport) uint64_t platform_dependency_value(void);
#else
#define EXPORT __attribute__((visibility("default")))
#define CALLBACK_CALL
extern uint64_t platform_dependency_value(void);
#endif

typedef void (CALLBACK_CALL *platform_callback_fn)(int32_t, const char*, void*);

EXPORT uint64_t platform_roundtrip(uint64_t value) {
    return value ^ platform_dependency_value();
}

EXPORT int32_t platform_emit_callback(platform_callback_fn callback, uintptr_t token) {
    char details[] = "{\"source\":\"platform-loader\",\"value\":9007199254740993}";
    callback(-12345, details, (void*)token);
    /* The Go target must copy details while the callback owns this buffer. */
    details[0] = 'X';
    return 7;
}
