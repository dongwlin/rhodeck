#pragma once

// Experimental C API. Not part of the supported public surface; it
// exists here so the checker can verify the boundary between the
// locked profile and the experimental exports.

#if ASST_WITH_EXTRA_CALLERS

#include "AsstCaller.h"

struct AsstMapLevelKey
{
    const char* stage_id;
    const char* code;
    const char* level_id;
    const char* name;
};

#ifdef __cplusplus
extern "C"
{
#endif

    // Looks up a stage by name, code, stageId or levelId. Returns a
    // struct by value; all fields are NULL when nothing matches.
    struct AsstMapLevelKey ASSTAPI AsstGetMapLevelKey(const char* key);

    // Looks up an item name by id. The returned string is owned by the
    // library and must be copied when kept across calls.
    const char* ASSTAPI AsstGetItemName(const char* id);

#ifdef __cplusplus
}
#endif

#endif // ASST_WITH_EXTRA_CALLERS
