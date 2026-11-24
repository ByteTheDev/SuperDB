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
  src/sdb_util.c
  src/sdb_crc.c
  src/sdb_store.c
  src/sdb_log.c
  src/superdb.c
)
target_include_directories(superdb PUBLIC include PRIVATE src)

# --- protocol + command dispatch (tools only) ---
add_library(sdbproto STATIC src/sdb_resp.c src/sdb_cmd.c)
target_include_directories(sdbproto PUBLIC src PRIVATE include)
target_link_libraries(sdbproto PUBLIC superdb)

find_package(Threads REQUIRED)

# --- server + cli ---
add_executable(superdb-server src/server.c)
target_include_directories(superdb-server PRIVATE src include)
target_link_libraries(superdb-server PRIVATE sdbproto superdb Threads::Threads)

add_executable(superdb-cli src/sdb_client.c src/cli.c)
target_include_directories(superdb-cli PRIVATE src include)
if(WIN32)
  target_link_libraries(superdb-cli PRIVATE ws2_32)
  target_link_libraries(superdb-server PRIVATE ws2_32)
endif()

# --- tests ---
enable_testing()
add_executable(test_basic tests/test_basic.c)
target_include_directories(test_basic PRIVATE include src)
target_link_libraries(test_basic PRIVATE superdb Threads::Threads)
add_test(NAME basic COMMAND test_basic)

add_executable(test_batch tests/test_batch.c)
target_include_directories(test_batch PRIVATE include src)
target_link_libraries(test_batch PRIVATE superdb Threads::Threads)
add_test(NAME batch COMMAND test_batch)

add_executable(test_resp tests/test_resp.c)
target_include_directories(test_resp PRIVATE src)
target_link_libraries(test_resp PRIVATE sdbproto)
# test_resp only needs sdb_resp; link the single object via library subset:
# (sdbproto pulls superdb — harmless for a pure-protocol test)
add_test(NAME resp COMMAND test_resp)

# --- install ---
include(GNUInstallDirs)
install(TARGETS superdb superdb-server superdb-cli sdbproto
        ARCHIVE DESTINATION ${CMAKE_INSTALL_LIBDIR}
        RUNTIME DESTINATION ${CMAKE_INSTALL_BINDIR})
install(FILES include/superdb.h DESTINATION ${CMAKE_INSTALL_INCLUDEDIR})
