#pragma once

// Calling convention of the exported Asst API functions.
// The frozen branch order is: Wine on x86_64, then _WIN32, then the
// system C ABI. External predefinition of ASST_CALL is rejected.
#if !defined(ASST_CALL)
#if defined(__WINE__) && defined(__x86_64__)
#define ASST_CALL __attribute__((sysv_abi))
#elif defined(_WIN32)
#define ASST_CALL __stdcall
#else
#define ASST_CALL
#endif /* __WINE__ / _WIN32 */
#endif /* ASST_CALL */

// Import/export decoration of library symbols. Windows builds use
// __declspec; GNU-compatible builds use visibility attributes; other
// builds get empty decorations.
#if (defined(_WIN32) || defined(__CYGWIN__)) && !defined(__WINE__)
#define ASST_DLL_IMPORT __declspec(dllimport)
#define ASST_DLL_EXPORT __declspec(dllexport)
#define ASST_DLL_LOCAL
#else
#if __GNUC__ >= 4
#define ASST_DLL_IMPORT __attribute__((visibility("default")))
#define ASST_DLL_EXPORT __attribute__((visibility("default")))
#define ASST_DLL_LOCAL __attribute__((visibility("hidden")))
#else
#define ASST_DLL_IMPORT
#define ASST_DLL_EXPORT
#define ASST_DLL_LOCAL
#endif
#endif

#ifdef ASST_DLL_EXPORTS // set when building the DLL instead of using it
#define ASSTAPI_PORT ASST_DLL_EXPORT
#else
#define ASSTAPI_PORT ASST_DLL_IMPORT
#endif // ASST_DLL_EXPORTS

#define ASSTAPI ASSTAPI_PORT ASST_CALL

#define ASSTLOCAL ASST_DLL_LOCAL ASST_CALL
