cmake_minimum_required(VERSION 3.15)
project(superdb VERSION 2.0.0 LANGUAGES C)

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

find_package(Threads REQUIRED)

# _POSIX_C_SOURCE exposes fileno/ftruncate/off_t & co. under strict -std=c99.
if(NOT WIN32)
  add_compile_definitions(_POSIX_C_SOURCE=200809L)
endif()

# --- core library: util + crc + thread + glob + coll + store + log + public API ---
add_library(superdb STATIC
  src/core/superdb_util.c
  src/core/superdb_crc.c
  src/core/superdb_thread.c
  src/core/superdb_glob.c
  src/core/superdb_coll.c
  src/core/superdb_store.c
  src/core/superdb_log.c
  src/core/superdb.c
)
target_include_directories(superdb PUBLIC include PRIVATE src/core)
target_link_libraries(superdb PUBLIC Threads::Threads)

# --- protocol + command dispatch + networking (tools only) ---
add_library(superdb_proto STATIC
  src/proto/superdb_resp.c src/proto/superdb_cmd.c
  src/net/superdb_net.c src/net/superdb_repl.c
)
target_include_directories(superdb_proto PUBLIC src/proto PRIVATE src/core src/net include)
target_link_libraries(superdb_proto PUBLIC superdb)
if(WIN32)
  target_link_libraries(superdb_proto PUBLIC ws2_32)
endif()

# --- server + cli + project tool ---
add_executable(superdb-server src/net/server.c)
target_include_directories(superdb-server PRIVATE src/core src/net src/proto include)
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
# core-only: basic, batch, typed values, transactions, compaction, replication
foreach(t basic batch types txn compact repl)
  add_executable(test_${t} tests/test_${t}.c)
  target_include_directories(test_${t} PRIVATE include src/core)
  target_link_libraries(test_${t} PRIVATE superdb Threads::Threads)
  add_test(NAME ${t} COMMAND test_${t})
endforeach()

# command layer: needs the command dispatcher and the networking/replica modules
foreach(t cmd fuzz)
  add_executable(test_${t} tests/test_${t}.c)
  target_include_directories(test_${t} PRIVATE include src src/core src/proto src/net)
  target_link_libraries(test_${t} PRIVATE superdb_proto superdb Threads::Threads)
  add_test(NAME ${t} COMMAND test_${t})
endforeach()

add_executable(test_resp tests/test_resp.c)
target_include_directories(test_resp PRIVATE src/proto)
target_link_libraries(test_resp PRIVATE superdb_proto)
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
