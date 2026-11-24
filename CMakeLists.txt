cmake_minimum_required(VERSION 3.15)
project(superdb VERSION 1.1.0 LANGUAGES C)

set(CMAKE_C_STANDARD 99)
set(CMAKE_C_STANDARD_REQUIRED ON)
set(CMAKE_C_EXTENSIONS OFF)

if(NOT CMAKE_BUILD_TYPE)
  set(CMAKE_BUILD_TYPE Release)
endif()
if(MSVC)
  add_compile_options(/W3)
else()
  add_compile_options(-Wall -Wextra)
endif()

# --- core library: util + crc + store + log + public API ---
add_library(superdb STATIC
  src/core/superdb_util.c
  src/core/superdb_crc.c
  src/core/superdb_store.c
  src/core/superdb_log.c
  src/core/superdb.c
)
target_include_directories(superdb PUBLIC include PRIVATE src/core)

# --- protocol + command dispatch (tools only) ---
add_library(superdb_proto STATIC src/proto/superdb_resp.c src/proto/superdb_cmd.c)
target_include_directories(superdb_proto PUBLIC src/proto PRIVATE include)
target_link_libraries(superdb_proto PUBLIC superdb)

find_package(Threads REQUIRED)

# --- server + cli + project tool ---
add_executable(superdb-server src/net/server.c)
target_include_directories(superdb-server PRIVATE src/net src/proto include)
target_link_libraries(superdb-server PRIVATE superdb_proto superdb Threads::Threads)

add_executable(superdb-cli src/net/superdb_client.c src/cli/cli.c)
target_include_directories(superdb-cli PRIVATE src/cli src/net include)

# `superdb` project manager / offline toolkit (links the core lib only)
add_executable(superdb-tool src/cli/superdb_tool.c)
set_target_properties(superdb-tool PROPERTIES OUTPUT_NAME superdb)
target_include_directories(superdb-tool PRIVATE include)
target_link_libraries(superdb-tool PRIVATE superdb Threads::Threads)
if(WIN32)
  target_link_libraries(superdb-cli PRIVATE ws2_32)
  target_link_libraries(superdb-server PRIVATE ws2_32)
  target_link_libraries(superdb-tool PRIVATE ws2_32)
endif()

# --- tests ---
enable_testing()
add_executable(test_basic tests/test_basic.c)
target_include_directories(test_basic PRIVATE include)
target_link_libraries(test_basic PRIVATE superdb Threads::Threads)
add_test(NAME basic COMMAND test_basic)

add_executable(test_batch tests/test_batch.c)
target_include_directories(test_batch PRIVATE include)
target_link_libraries(test_batch PRIVATE superdb Threads::Threads)
add_test(NAME batch COMMAND test_batch)

add_executable(test_resp tests/test_resp.c)
target_include_directories(test_resp PRIVATE src/proto)
target_link_libraries(test_resp PRIVATE superdb_proto)
# test_resp only needs superdb_resp; link the single object via library subset:
# (superdb_proto pulls superdb — harmless for a pure-protocol test)
add_test(NAME resp COMMAND test_resp)

# tool roundtrip (offline kv + dump/restore + init/install); needs the
# just-built `superdb` binary plus a POSIX sh.
find_program(SH_BIN sh)
if(SH_BIN)
  add_test(NAME tool
    COMMAND ${SH_BIN} ${CMAKE_CURRENT_SOURCE_DIR}/tests/test_tool.sh
            --bindir ${CMAKE_CURRENT_BINARY_DIR}
            --srcdir ${CMAKE_CURRENT_SOURCE_DIR})
endif()

# --- install ---
include(GNUInstallDirs)
install(TARGETS superdb superdb-server superdb-cli superdb-tool superdb_proto
        ARCHIVE DESTINATION ${CMAKE_INSTALL_LIBDIR}
        RUNTIME DESTINATION ${CMAKE_INSTALL_BINDIR})
install(FILES include/superdb.h DESTINATION ${CMAKE_INSTALL_INCLUDEDIR})
