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

# --- core library: util + crc + thread + glob + coll + store + log + public API ---
add_library(superdb STATIC
  src/sdb_util.c
  src/sdb_crc.c
  src/sdb_thread.c
  src/sdb_glob.c
  src/sdb_coll.c
  src/sdb_store.c
  src/sdb_log.c
  src/superdb.c
)
target_include_directories(superdb PUBLIC include PRIVATE src)
target_link_libraries(superdb PUBLIC Threads::Threads)

# --- protocol + command dispatch + networking (tools only) ---
add_library(sdbproto STATIC src/sdb_resp.c src/sdb_cmd.c src/sdb_net.c src/sdb_repl.c)
target_include_directories(sdbproto PUBLIC src PRIVATE include)
target_link_libraries(sdbproto PUBLIC superdb)
if(WIN32)
  target_link_libraries(sdbproto PUBLIC ws2_32)
endif()

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
foreach(t basic batch types txn compact repl)
  add_executable(test_${t} tests/test_${t}.c)
  target_include_directories(test_${t} PRIVATE include src)
  target_link_libraries(test_${t} PRIVATE superdb Threads::Threads)
  add_test(NAME ${t} COMMAND test_${t})
endforeach()

foreach(t cmd fuzz)
  add_executable(test_${t} tests/test_${t}.c)
  target_include_directories(test_${t} PRIVATE include src)
  target_link_libraries(test_${t} PRIVATE sdbproto superdb Threads::Threads)
  add_test(NAME ${t} COMMAND test_${t})
endforeach()

add_executable(test_resp tests/test_resp.c)
target_include_directories(test_resp PRIVATE src)
target_link_libraries(test_resp PRIVATE sdbproto)
add_test(NAME resp COMMAND test_resp)

# --- install ---
include(GNUInstallDirs)
install(TARGETS superdb superdb-server superdb-cli sdbproto
        ARCHIVE DESTINATION ${CMAKE_INSTALL_LIBDIR}
        RUNTIME DESTINATION ${CMAKE_INSTALL_BINDIR})
install(FILES include/superdb.h DESTINATION ${CMAKE_INSTALL_INCLUDEDIR})
