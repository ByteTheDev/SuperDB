@echo off
REM Build SuperDB on Windows with gcc (MSYS2 ucrt64 / MinGW). No make needed.
REM Modules: core (util/crc/thread/glob/coll/store/log/api) + proto + cmd +
REM net/repl (replica) + server/cli. Then all unit tests, quick benches, examples.

set CFLAGS=-std=c99 -O2 -Wall -Wextra -Iinclude -Isrc
set CORE=src\sdb_util.o src\sdb_crc.o src\sdb_thread.o src\sdb_glob.o src\sdb_coll.o src\sdb_store.o src\sdb_log.o src\superdb.o
set CMDOBJ=src\sdb_resp.o src\sdb_cmd.o src\sdb_net.o src\sdb_repl.o

mkdir bin 2>nul
mkdir lib 2>nul

gcc %CFLAGS% -c src\sdb_util.c -o src\sdb_util.o
if errorlevel 1 exit /b 1
gcc %CFLAGS% -c src\sdb_crc.c -o src\sdb_crc.o
if errorlevel 1 exit /b 1
gcc %CFLAGS% -c src\sdb_thread.c -o src\sdb_thread.o
if errorlevel 1 exit /b 1
gcc %CFLAGS% -c src\sdb_glob.c -o src\sdb_glob.o
if errorlevel 1 exit /b 1
gcc %CFLAGS% -c src\sdb_coll.c -o src\sdb_coll.o
if errorlevel 1 exit /b 1
gcc %CFLAGS% -c src\sdb_store.c -o src\sdb_store.o
if errorlevel 1 exit /b 1
gcc %CFLAGS% -c src\sdb_log.c -o src\sdb_log.o
if errorlevel 1 exit /b 1
gcc %CFLAGS% -c src\superdb.c -o src\superdb.o
if errorlevel 1 exit /b 1
gcc %CFLAGS% -c src\sdb_resp.c -o src\sdb_resp.o
if errorlevel 1 exit /b 1
gcc %CFLAGS% -c src\sdb_cmd.c -o src\sdb_cmd.o
if errorlevel 1 exit /b 1
gcc %CFLAGS% -c src\sdb_net.c -o src\sdb_net.o
if errorlevel 1 exit /b 1
gcc %CFLAGS% -c src\sdb_repl.c -o src\sdb_repl.o
if errorlevel 1 exit /b 1
gcc %CFLAGS% -c src\sdb_client.c -o src\sdb_client.o
if errorlevel 1 exit /b 1

ar rcs lib\libsuperdb.a %CORE%
if errorlevel 1 exit /b 1

gcc %CFLAGS% src\server.c %CMDOBJ% %CORE% -o bin\superdb-server.exe -lws2_32
if errorlevel 1 exit /b 1
gcc %CFLAGS% src\cli.c src\sdb_client.o -o bin\superdb-cli.exe -lws2_32
if errorlevel 1 exit /b 1

gcc %CFLAGS% tests\test_basic.c %CORE% -o tests\test_basic.exe
if errorlevel 1 exit /b 1
gcc %CFLAGS% tests\test_batch.c %CORE% -o tests\test_batch.exe
if errorlevel 1 exit /b 1
gcc %CFLAGS% tests\test_resp.c src\sdb_resp.o -o tests\test_resp.exe
if errorlevel 1 exit /b 1
gcc %CFLAGS% tests\test_types.c %CORE% -o tests\test_types.exe
if errorlevel 1 exit /b 1
gcc %CFLAGS% tests\test_txn.c %CORE% -o tests\test_txn.exe
if errorlevel 1 exit /b 1
gcc %CFLAGS% tests\test_compact.c %CORE% -o tests\test_compact.exe
if errorlevel 1 exit /b 1
gcc %CFLAGS% tests\test_repl.c %CORE% -o tests\test_repl.exe
if errorlevel 1 exit /b 1
gcc %CFLAGS% tests\test_cmd.c src\sdb_resp.o src\sdb_cmd.o %CORE% -o tests\test_cmd.exe -lws2_32
if errorlevel 1 exit /b 1
gcc %CFLAGS% tests\test_fuzz.c src\sdb_resp.o src\sdb_cmd.o %CORE% -o tests\test_fuzz.exe -lws2_32
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
