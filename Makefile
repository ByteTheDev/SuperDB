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
CPPFLAGS += -Iinclude -Isrc
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
CORE_SRCS  := src/sdb_util.c src/sdb_crc.c src/sdb_thread.c src/sdb_glob.c \
              src/sdb_coll.c src/sdb_store.c src/sdb_log.c src/superdb.c
PROTO_SRCS := src/sdb_resp.c
CMD_SRCS   := src/sdb_cmd.c
NET_SRCS   := src/sdb_net.c src/sdb_repl.c
SERVER_SRCS := src/server.c
CLIENT_SRCS := src/sdb_client.c src/cli.c

CORE_OBJS  := $(CORE_SRCS:.c=.o)
PROTO_OBJS := $(PROTO_SRCS:.c=.o)
CMD_OBJS   := $(CMD_SRCS:.c=.o)
NET_OBJS   := $(NET_SRCS:.c=.o)

SERVER_OBJS := $(SERVER_SRCS:.c=.o) $(CORE_OBJS) $(PROTO_OBJS) $(CMD_OBJS) $(NET_OBJS)
CLI_OBJS    := src/sdb_client.o src/cli.o

BIN_SERVER := bin/superdb-server$(EXE)
BIN_CLI    := bin/superdb-cli$(EXE)
LIB_STATIC := lib/libsuperdb.a

TESTS := tests/test_basic$(EXE) tests/test_batch$(EXE) tests/test_resp$(EXE) \
         tests/test_types$(EXE) tests/test_txn$(EXE) tests/test_cmd$(EXE) \
         tests/test_compact$(EXE) tests/test_repl$(EXE) tests/test_fuzz$(EXE)
BENCH_SDB := bench/bench_sdb$(EXE)
EXAMPLES := examples/example$(EXE) examples/batch$(EXE)

ALL_OBJS := $(CORE_OBJS) $(PROTO_OBJS) $(CMD_OBJS) $(NET_OBJS) $(SERVER_SRCS:.c=.o) $(CLIENT_SRCS:.c=.o)

all: $(BIN_SERVER) $(BIN_CLI)

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

# --- tests ---
tests/test_basic$(EXE): tests/test_basic.c $(CORE_OBJS)
	$(CC) $(CPPFLAGS) $(CFLAGS) $^ -o $@ $(LDFLAGS) $(LDLIBS)

tests/test_batch$(EXE): tests/test_batch.c $(CORE_OBJS)
	$(CC) $(CPPFLAGS) $(CFLAGS) $^ -o $@ $(LDFLAGS) $(LDLIBS)

tests/test_resp$(EXE): tests/test_resp.c $(PROTO_OBJS)
	$(CC) $(CPPFLAGS) $(CFLAGS) $^ -o $@ $(LDFLAGS)

# typed values, transactions, commands, replication, fuzzing: core + command layer
CMD_TEST_OBJS := $(CORE_OBJS) $(PROTO_OBJS) $(CMD_OBJS) $(NET_OBJS)

tests/test_types$(EXE): tests/test_types.c $(CORE_OBJS)
	$(CC) $(CPPFLAGS) $(CFLAGS) $^ -o $@ $(LDFLAGS) $(LDLIBS)

tests/test_txn$(EXE): tests/test_txn.c $(CORE_OBJS)
	$(CC) $(CPPFLAGS) $(CFLAGS) $^ -o $@ $(LDFLAGS) $(LDLIBS)

tests/test_compact$(EXE): tests/test_compact.c $(CORE_OBJS)
	$(CC) $(CPPFLAGS) $(CFLAGS) $^ -o $@ $(LDFLAGS) $(LDLIBS)

tests/test_repl$(EXE): tests/test_repl.c $(CORE_OBJS)
	$(CC) $(CPPFLAGS) $(CFLAGS) $^ -o $@ $(LDFLAGS) $(LDLIBS)

tests/test_cmd$(EXE): tests/test_cmd.c $(CMD_TEST_OBJS)
	$(CC) $(CPPFLAGS) $(CFLAGS) $^ -o $@ $(LDFLAGS) $(LDLIBS)

tests/test_fuzz$(EXE): tests/test_fuzz.c $(CMD_TEST_OBJS)
	$(CC) $(CPPFLAGS) $(CFLAGS) $^ -o $@ $(LDFLAGS) $(LDLIBS)

test check: $(BIN_SERVER) $(BIN_CLI) $(TESTS)
	./tests/test_basic$(EXE)
	./tests/test_batch$(EXE)
	./tests/test_resp$(EXE)
	./tests/test_types$(EXE)
	./tests/test_txn$(EXE)
	./tests/test_compact$(EXE)
	./tests/test_repl$(EXE)
	./tests/test_cmd$(EXE)
	./tests/test_fuzz$(EXE)

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
	cp $(BIN_SERVER) $(BIN_CLI) $(DESTDIR)$(BINDIR)/
	cp $(LIB_STATIC) $(DESTDIR)$(LIBDIR)/
	cp include/superdb.h $(DESTDIR)$(INCDIR)/

clean:
	rm -f $(ALL_OBJS) $(ALL_OBJS:.o=.d)
	rm -f tests/*.o tests/*.d bench/*.o bench/*.d examples/*.o examples/*.d
	rm -f $(TESTS) $(BENCH_SDB) $(EXAMPLES) $(LIB_STATIC)
	rm -f *.sdb *.sdb.tmp bench_*.sdb* test_*.sdb* *.db *.db-wal *.db-shm *.db-journal

.PHONY: all lib test check bench examples install clean

-include $(ALL_OBJS:.o=.d)
