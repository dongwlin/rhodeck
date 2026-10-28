#include <stdint.h>

#ifdef _WIN32
#define EXPORT __declspec(dllexport)
#else
#define EXPORT __attribute__((visibility("default")))
#endif

EXPORT uint64_t platform_dependency_value(void) {
    return UINT64_C(0xfedcba9876543210);
}
