# SuperDB build — modular C99, no dependencies besides libc
# (POSIX: threads from libc; Windows: ws2_32 for the tools).
#
#   make            server + cli
#   make test       all unit tests (core, batch, protocol)
#   make bench      quick perf check (nosync + durable batch)
#   make examples   example programs
#   make install    install headers/lib/bins (PREFIX, default /usr/local)
#   make clean

CC      ?= gcc
CFLAGS  ?= -std=c99 -O2 -Wall -Wextra
# _POSIX_C_SOURCE exposes fileno/ftruncate/off_t & co. under strict -std=c99
# (glibc hides them in strict-ANSI mode). Harmless on Windows toolchains.
CPPFLAGS += -Iinclude -Isrc -Isrc/core -Isrc/proto -Isrc/net -D_POSIX_C_SOURCE=200809L
LDFLAGS ?=
LDLIBS  ?=
DEPFLAGS = -MMD -MP

PREFIX  ?= /usr/local
BINDIR  ?= $(PREFIX)/bin
LIBDIR  ?= $(PREFIX)/lib
INCDIR  ?= $(PREFIX)/include

UNAME_S := $(shell uname -s 2>/dev/null || echo Windows)
ifeq ($(OS),Windows_NT)
  EXE    := .exe
  LDLIBS += -lws2_32
else
  EXE    :=
  LDLIBS += -lpthread
endif

# --- module groups (see docs/ARCHITECTURE.md) ---
CORE_SRCS  := src/core/superdb_util.c src/core/superdb_crc.c src/core/superdb_store.c src/core/superdb_log.c src/core/superdb.c
PROTO_SRCS := src/proto/superdb_resp.c
CMD_SRCS   := src/proto/superdb_cmd.c
SERVER_SRCS := src/net/server.c
TOOL_SRCS   := src/cli/superdb_tool.c
CLIENT_SRCS := src/net/superdb_client.c src/cli/cli.c

CORE_OBJS  := $(CORE_SRCS:.c=.o)
PROTO_OBJS := $(PROTO_SRCS:.c=.o)
CMD_OBJS   := $(CMD_SRCS:.c=.o)

SERVER_OBJS := $(SERVER_SRCS:.c=.o) $(CORE_OBJS) $(PROTO_OBJS) $(CMD_OBJS)
TOOL_OBJS   := $(TOOL_SRCS:.c=.o) $(CORE_OBJS)
CLI_OBJS    := $(CLIENT_SRCS:.c=.o)

BIN_SERVER := bin/superdb-server$(EXE)
BIN_CLI    := bin/superdb-cli$(EXE)
BIN_TOOL   := bin/superdb$(EXE)
LIB_STATIC := lib/libsuperdb.a

TESTS := tests/test_basic$(EXE) tests/test_batch$(EXE) tests/test_resp$(EXE)
BENCH_SDB := bench/bench_sdb$(EXE)
EXAMPLES := examples/example$(EXE) examples/batch$(EXE)

ALL_OBJS := $(CORE_OBJS) $(PROTO_OBJS) $(CMD_OBJS) $(SERVER_SRCS:.c=.o) $(TOOL_SRCS:.c=.o) $(CLIENT_SRCS:.c=.o)

all: $(BIN_SERVER) $(BIN_CLI) $(BIN_TOOL)

lib: $(LIB_STATIC)

%.o: %.c
	$(CC) $(CPPFLAGS) $(CFLAGS) $(DEPFLAGS) -c $< -o $@

$(LIB_STATIC): $(CORE_OBJS)
	mkdir -p lib
	$(AR) rcs $@ $^

$(BIN_SERVER): $(SERVER_OBJS)
	$(CC) $(CPPFLAGS) $(CFLAGS) $^ -o $@ $(LDFLAGS) $(LDLIBS)

$(BIN_CLI): $(CLI_OBJS)
	$(CC) $(CPPFLAGS) $(CFLAGS) $^ -o $@ $(LDFLAGS) $(LDLIBS)

$(BIN_TOOL): $(TOOL_OBJS)
	$(CC) $(CPPFLAGS) $(CFLAGS) $^ -o $@ $(LDFLAGS) $(LDLIBS)

# --- tests ---
tests/test_basic$(EXE): tests/test_basic.c $(CORE_OBJS)
	$(CC) $(CPPFLAGS) $(CFLAGS) $^ -o $@ $(LDFLAGS) $(LDLIBS)

tests/test_batch$(EXE): tests/test_batch.c $(CORE_OBJS)
	$(CC) $(CPPFLAGS) $(CFLAGS) $^ -o $@ $(LDFLAGS) $(LDLIBS)

tests/test_resp$(EXE): tests/test_resp.c $(PROTO_OBJS)
	$(CC) $(CPPFLAGS) $(CFLAGS) $^ -o $@ $(LDFLAGS)

test check: $(BIN_SERVER) $(BIN_CLI) $(BIN_TOOL) $(TESTS)
	./tests/test_basic$(EXE)
	./tests/test_batch$(EXE)
	./tests/test_resp$(EXE)
	sh tests/test_tool.sh

# --- benchmarks & examples ---
$(BENCH_SDB): bench/bench_sdb.c $(CORE_OBJS)
	$(CC) $(CPPFLAGS) $(CFLAGS) $^ -o $@ $(LDFLAGS) $(LDLIBS)

bench: $(BENCH_SDB)
	./$(BENCH_SDB) --n 100000 --nosync --skip-mixed
	./$(BENCH_SDB) --n 100000 --sync --batch 1000 --skip-mixed

examples/example$(EXE): examples/example.c $(CORE_OBJS)
	$(CC) $(CPPFLAGS) $(CFLAGS) $^ -o $@ $(LDFLAGS) $(LDLIBS)

examples/batch$(EXE): examples/batch.c $(CORE_OBJS)
	$(CC) $(CPPFLAGS) $(CFLAGS) $^ -o $@ $(LDFLAGS) $(LDLIBS)

examples: $(EXAMPLES)

# --- install ---
install: all $(LIB_STATIC)
	mkdir -p $(DESTDIR)$(BINDIR) $(DESTDIR)$(LIBDIR) $(DESTDIR)$(INCDIR)
	cp $(BIN_SERVER) $(BIN_CLI) $(BIN_TOOL) $(DESTDIR)$(BINDIR)/
	cp $(LIB_STATIC) $(DESTDIR)$(LIBDIR)/
	cp include/superdb.h $(DESTDIR)$(INCDIR)/

clean:
	rm -f $(ALL_OBJS) $(ALL_OBJS:.o=.d)
	rm -f tests/*.o tests/*.d bench/*.o bench/*.d examples/*.o examples/*.d
	rm -f $(TESTS) $(BENCH_SDB) $(EXAMPLES) $(LIB_STATIC)
	rm -f *.sdb *.sdb.tmp bench_*.sdb* test_*.sdb* *.db *.db-wal *.db-shm *.db-journal

.PHONY: all lib test check bench examples install clean

-include $(ALL_OBJS:.o=.d)
