import json
import os
import shutil
import socket
import sqlite3
import subprocess
import tempfile
import time

ROWS = 10_000
BENCH_BINARY = os.path.join(tempfile.gettempdir(), "superdb-benchmark.exe")


def read_exact(sock, size):
    result = bytearray()
    while len(result) < size:
        chunk = sock.recv(size - len(result))
        if not chunk:
            raise RuntimeError("connection closed")
        result.extend(chunk)
    return result


def send_queries(sock, queries, batched=True):
    if batched:
        payload = json.dumps({"sqls": queries}, separators=(",", ":")).encode()
        sock.sendall(len(payload).to_bytes(4, "big") + payload)
        header = read_exact(sock, 4)
        return json.loads(read_exact(sock, int.from_bytes(header, "big")))
    results = []
    for query in queries:
        payload = json.dumps({"sql": query}, separators=(",", ":")).encode()
        sock.sendall(len(payload).to_bytes(4, "big") + payload)
        header = read_exact(sock, 4)
        results.append(json.loads(read_exact(sock, int.from_bytes(header, "big"))))
    return results


def start_superdb(mode="memory", data=None):
    probe = socket.socket()
    probe.bind(("127.0.0.1", 0))
    port = probe.getsockname()[1]
    probe.close()
    command = [BENCH_BINARY, "server", "--addr", f"127.0.0.1:{port}", "--mode", mode]
    if data:
        command += ["--data", data]
    process = subprocess.Popen(
        command,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    )
    deadline = time.perf_counter() + 30
    while time.perf_counter() < deadline:
        try:
            sock = socket.create_connection(("127.0.0.1", port), timeout=1)
            sock.settimeout(60)
            return process, sock
        except OSError:
            time.sleep(0.05)
    process.terminate()
    process.wait(timeout=10)
    raise RuntimeError("SuperDB did not start")


def superdb_run(setup, workload, batched=True, mode="memory", data=None):
    process, sock = start_superdb(mode, data)
    try:
        send_queries(sock, setup, batched=True)
        started = time.perf_counter()
        send_queries(sock, workload, batched=batched)
        return time.perf_counter() - started
    finally:
        sock.close()
        process.terminate()
        process.wait(timeout=10)


def sqlite_run(setup, workload, executemany=False):
    db = sqlite3.connect(":memory:")
    for query in setup:
        db.execute(query)
    started = time.perf_counter()
    if executemany and workload and workload[0].startswith("INSERT INTO"):
        db.executemany(
            "INSERT INTO users VALUES (?, ?, ?)",
            ((i, f"user-{i}", i % 100) for i in range(len(workload))),
        )
    else:
        for query in workload:
            if query.upper().startswith("SELECT"):
                db.execute(query).fetchall()
            else:
                db.execute(query)
    db.commit()
    return time.perf_counter() - started


def benchmark(name, setup, workload, sqlite_executemany=False):
    superdb = superdb_run(setup, workload, batched=True)
    sqlite = sqlite_run(setup, workload, executemany=sqlite_executemany)
    count = len(workload)
    print(f"{name:<20} SuperDB {superdb:>7.3f}s {count / superdb:>10.0f} ops/s | SQLite {sqlite:>7.3f}s {count / sqlite:>10.0f} ops/s | {superdb / sqlite:>5.2f}x")


def durable_wal_benchmark(count=1_000):
    setup = ["CREATE TABLE users (id INT PRIMARY KEY, name TEXT, score INT)"]
    workload = [f"INSERT INTO users VALUES ({i}, 'user-{i}', {i % 100})" for i in range(count)]

    superdb_dir = tempfile.mkdtemp(prefix="superdb-wal-")
    sqlite_dir = tempfile.mkdtemp(prefix="sqlite-wal-")
    try:
        superdb = superdb_run(setup, workload, batched=True, mode="wal", data=superdb_dir)
        sqlite_path = os.path.join(sqlite_dir, "database.sqlite")
        db = sqlite3.connect(sqlite_path)
        db.execute("PRAGMA journal_mode=WAL")
        db.execute("PRAGMA synchronous=FULL")
        db.execute("CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT, score INTEGER)")
        started = time.perf_counter()
        for i in range(count):
            db.execute("INSERT INTO users VALUES (?, ?, ?)", (i, f"user-{i}", i % 100))
        db.commit()
        sqlite = time.perf_counter() - started
        db.close()
        print(f"{'durable WAL inserts':<20} SuperDB {superdb:>7.3f}s {count / superdb:>10.0f} ops/s | SQLite {sqlite:>7.3f}s {count / sqlite:>10.0f} ops/s | {superdb / sqlite:>5.2f}x")
    finally:
        shutil.rmtree(superdb_dir, ignore_errors=True)
        shutil.rmtree(sqlite_dir, ignore_errors=True)


if __name__ == "__main__":
    subprocess.run(["go", "build", "-o", BENCH_BINARY, "./cmd/superdb"], check=True)
    setup = ["CREATE TABLE users (id INT PRIMARY KEY, name TEXT, score INT)"]
    inserts = [f"INSERT INTO users VALUES ({i}, 'user-{i}', {i % 100})" for i in range(ROWS)]
    selects = [f"SELECT * FROM users WHERE id = {i % ROWS}" for i in range(ROWS)]
    scans = ["SELECT * FROM users WHERE score = 42" for _ in range(1_000)]
    updates = [f"UPDATE users SET score = {(i + 1) % 100} WHERE id = {i}" for i in range(5_000)]
    deletes = [f"DELETE FROM users WHERE id = {i}" for i in range(5_000)]
    mixed = []
    for i in range(2_000):
        mixed.extend([
            f"INSERT INTO users VALUES ({ROWS + i}, 'new-{i}', {i % 100})",
            f"SELECT * FROM users WHERE id = {i}",
            f"UPDATE users SET score = {(i + 7) % 100} WHERE id = {i}",
        ])
    indexed_setup = setup + inserts + ["CREATE INDEX users_score ON users (score)"]
    indexed_reads = ["SELECT * FROM users WHERE score = 42" for _ in range(1_000)]
    aggregates = ["SELECT COUNT(*) FROM users WHERE score = 42" for _ in range(1_000)]
    multi_insert_setup = ["CREATE TABLE users (id INT PRIMARY KEY, name TEXT, score INT)"]
    multi_inserts = []
    for batch in range(100):
        values = ", ".join(
            f"({batch * 100 + i}, 'user-{batch * 100 + i}', {i % 100})"
            for i in range(100)
        )
        multi_inserts.append(f"INSERT INTO users VALUES {values}")

    print(f"SQLite {sqlite3.sqlite_version}; {ROWS:,}-row test data")
    print("Workloads are timed after setup; SuperDB uses one batched TCP request.")
    print("ratio < 1.00 means SuperDB was faster. Times include Python client work.")
    benchmark("bulk inserts", setup, inserts, sqlite_executemany=True)
    benchmark("primary-key reads", setup + inserts, selects)
    benchmark("filtered scans", setup + inserts, scans)
    benchmark("updates", setup + inserts, updates)
    benchmark("deletes", setup + inserts, deletes)
    benchmark("mixed workload", setup + inserts, mixed)
    benchmark("indexed reads", indexed_setup, indexed_reads)
    benchmark("aggregates", setup + inserts, aggregates)
    benchmark("multi-row inserts", multi_insert_setup, multi_inserts)
    durable_wal_benchmark()
