@echo off
REM Build SuperDB on Windows with gcc (MSYS2 ucrt64 / MinGW). No make needed.
REM Modules: core (util/crc/store/log/api) + proto + cmd + server/cli.

set CFLAGS=-std=c99 -O2 -Wall -Wextra -Iinclude -Isrc
set CORE=src\sdb_util.o src\sdb_crc.o src\sdb_store.o src\sdb_log.o src\superdb.o

mkdir bin 2>nul
mkdir lib 2>nul

gcc %CFLAGS% -c src\sdb_util.c -o src\sdb_util.o
if errorlevel 1 exit /b 1
gcc %CFLAGS% -c src\sdb_crc.c -o src\sdb_crc.o
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
gcc %CFLAGS% -c src\sdb_client.c -o src\sdb_client.o
if errorlevel 1 exit /b 1

ar rcs lib\libsuperdb.a %CORE%
if errorlevel 1 exit /b 1

gcc %CFLAGS% src\server.c src\sdb_resp.o src\sdb_cmd.o %CORE% -o bin\superdb-server.exe -lws2_32
if errorlevel 1 exit /b 1
gcc %CFLAGS% src\cli.c src\sdb_client.o -o bin\superdb-cli.exe -lws2_32
if errorlevel 1 exit /b 1

gcc %CFLAGS% tests\test_basic.c %CORE% -o tests\test_basic.exe
if errorlevel 1 exit /b 1
gcc %CFLAGS% tests\test_batch.c %CORE% -o tests\test_batch.exe
if errorlevel 1 exit /b 1
gcc %CFLAGS% tests\test_resp.c src\sdb_resp.o -o tests\test_resp.exe
if errorlevel 1 exit /b 1

tests\test_basic.exe
if errorlevel 1 exit /b 1
tests\test_batch.exe
if errorlevel 1 exit /b 1
tests\test_resp.exe
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
