@echo off
REM Build SuperDB on Windows with gcc (MSYS2 ucrt64 / MinGW). No make needed.
REM Modules: core/ (util/crc/thread/glob/coll/store/log/api) + proto/ + net/
REM (sockets, replication) + cli/ (repl + project tool). Then all unit tests,
REM the project-tool roundtrip, quick benches and examples.

set CFLAGS=-std=c99 -O2 -Wall -Wextra -Iinclude -Isrc -Isrc/core -Isrc/proto -Isrc/net -Isrc/cli -D_POSIX_C_SOURCE=200809L
set CORE=src\core\superdb_util.o src\core\superdb_crc.o src\core\superdb_thread.o src\core\superdb_glob.o src\core\superdb_coll.o src\core\superdb_store.o src\core\superdb_log.o src\core\superdb.o
set CMDOBJ=src\proto\superdb_resp.o src\proto\superdb_cmd.o
set NETOBJ=src\net\superdb_net.o src\net\superdb_repl.o

mkdir bin 2>nul
mkdir lib 2>nul

gcc %CFLAGS% -c src\core\superdb_util.c -o src\core\superdb_util.o
if errorlevel 1 exit /b 1
gcc %CFLAGS% -c src\core\superdb_crc.c -o src\core\superdb_crc.o
if errorlevel 1 exit /b 1
gcc %CFLAGS% -c src\core\superdb_thread.c -o src\core\superdb_thread.o
if errorlevel 1 exit /b 1
gcc %CFLAGS% -c src\core\superdb_glob.c -o src\core\superdb_glob.o
if errorlevel 1 exit /b 1
gcc %CFLAGS% -c src\core\superdb_coll.c -o src\core\superdb_coll.o
if errorlevel 1 exit /b 1
gcc %CFLAGS% -c src\core\superdb_store.c -o src\core\superdb_store.o
if errorlevel 1 exit /b 1
gcc %CFLAGS% -c src\core\superdb_log.c -o src\core\superdb_log.o
if errorlevel 1 exit /b 1
gcc %CFLAGS% -c src\core\superdb.c -o src\core\superdb.o
if errorlevel 1 exit /b 1
gcc %CFLAGS% -c src\proto\superdb_resp.c -o src\proto\superdb_resp.o
if errorlevel 1 exit /b 1
gcc %CFLAGS% -c src\proto\superdb_cmd.c -o src\proto\superdb_cmd.o
if errorlevel 1 exit /b 1
gcc %CFLAGS% -c src\net\superdb_net.c -o src\net\superdb_net.o
if errorlevel 1 exit /b 1
gcc %CFLAGS% -c src\net\superdb_repl.c -o src\net\superdb_repl.o
if errorlevel 1 exit /b 1
gcc %CFLAGS% -c src\net\superdb_client.c -o src\net\superdb_client.o
if errorlevel 1 exit /b 1
gcc %CFLAGS% -c src\cli\superdb_tool.c -o src\cli\superdb_tool.o
if errorlevel 1 exit /b 1

ar rcs lib\libsuperdb.a %CORE%
if errorlevel 1 exit /b 1

gcc %CFLAGS% src\net\server.c %CMDOBJ% %NETOBJ% %CORE% -o bin\superdb-server.exe -lws2_32
if errorlevel 1 exit /b 1
gcc %CFLAGS% src\cli\cli.c src\net\superdb_client.o -o bin\superdb-cli.exe -lws2_32
if errorlevel 1 exit /b 1
gcc %CFLAGS% src\cli\superdb_tool.o %CORE% -o bin\superdb.exe -lws2_32
if errorlevel 1 exit /b 1

gcc %CFLAGS% tests\test_basic.c %CORE% -o tests\test_basic.exe
if errorlevel 1 exit /b 1
gcc %CFLAGS% tests\test_batch.c %CORE% -o tests\test_batch.exe
if errorlevel 1 exit /b 1
gcc %CFLAGS% tests\test_resp.c src\proto\superdb_resp.o -o tests\test_resp.exe
if errorlevel 1 exit /b 1
gcc %CFLAGS% tests\test_types.c %CORE% -o tests\test_types.exe
if errorlevel 1 exit /b 1
gcc %CFLAGS% tests\test_txn.c %CORE% -o tests\test_txn.exe
if errorlevel 1 exit /b 1
gcc %CFLAGS% tests\test_compact.c %CORE% -o tests\test_compact.exe
if errorlevel 1 exit /b 1
gcc %CFLAGS% tests\test_repl.c %CORE% -o tests\test_repl.exe
if errorlevel 1 exit /b 1
gcc %CFLAGS% tests\test_cmd.c %CMDOBJ% %NETOBJ% %CORE% -o tests\test_cmd.exe -lws2_32
if errorlevel 1 exit /b 1
gcc %CFLAGS% tests\test_fuzz.c %CMDOBJ% %NETOBJ% %CORE% -o tests\test_fuzz.exe -lws2_32
if errorlevel 1 exit /b 1

tests\test_basic.exe
if errorlevel 1 exit /b 1
tests\test_batch.exe
if errorlevel 1 exit /b 1
tests\test_resp.exe
if errorlevel 1 exit /b 1
tests\test_types.exe
if errorlevel 1 exit /b 1
tests\test_txn.exe
if errorlevel 1 exit /b 1
tests\test_compact.exe
if errorlevel 1 exit /b 1
tests\test_repl.exe
if errorlevel 1 exit /b 1
tests\test_cmd.exe
if errorlevel 1 exit /b 1
tests\test_fuzz.exe
if errorlevel 1 exit /b 1

REM --- superdb project-tool roundtrip (offline, no server) ---
rmdir /s /q .tooltest 2>nul
bin\superdb.exe init .tooltest --db ./data.sdb --port 7381
if errorlevel 1 exit /b 1
bin\superdb.exe install .tooltest --from .
if errorlevel 1 exit /b 1
cd .tooltest
..\bin\superdb.exe --db ./data.sdb set greeting hello
if errorlevel 1 exit /b 1
..\bin\superdb.exe --db ./data.sdb dump --out dump.json
if errorlevel 1 exit /b 1
del data.sdb
..\bin\superdb.exe --db ./data.sdb restore --in dump.json
if errorlevel 1 exit /b 1
..\bin\superdb.exe --db ./data.sdb inspect >nul
if errorlevel 1 exit /b 1
cd ..
rmdir /s /q .tooltest

gcc %CFLAGS% bench\bench_sdb.c %CORE% -o bench\bench_sdb.exe
if errorlevel 1 exit /b 1
bench\bench_sdb.exe --n 100000 --nosync --skip-mixed
if errorlevel 1 exit /b 1
bench\bench_sdb.exe --n 100000 --sync --batch 1000 --skip-mixed
if errorlevel 1 exit /b 1

gcc %CFLAGS% examples\example.c %CORE% -o examples\example.exe
if errorlevel 1 exit /b 1
gcc %CFLAGS% examples\batch.c %CORE% -o examples\batch.exe
if errorlevel 1 exit /b 1

echo BUILD OK
