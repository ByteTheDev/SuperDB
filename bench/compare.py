import json
import socket
import sqlite3
import subprocess
import sys
import time

ROWS = 10_000
SELECTS = 5_000
PORT = 17654


def superdb_run():
    process = subprocess.Popen(
        ["go", "run", "./cmd/superdb", "server", "--addr", f"127.0.0.1:{PORT}"],
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    )
    try:
        deadline = time.perf_counter() + 30
        while time.perf_counter() < deadline:
            try:
                sock = socket.create_connection(("127.0.0.1", PORT), timeout=1)
                break
            except OSError:
                time.sleep(0.05)
        else:
            raise RuntimeError("SuperDB did not start")

        queries = ["CREATE TABLE users (id INT PRIMARY KEY, name TEXT)"]
        queries += [f"INSERT INTO users VALUES ({i}, 'user-{i}')" for i in range(ROWS)]
        queries += [f"SELECT * FROM users WHERE id = {i}" for i in range(SELECTS)]
        started = time.perf_counter()
        for query in queries:
            payload = json.dumps({"sql": query}).encode()
            sock.sendall(len(payload).to_bytes(4, "big") + payload)
        for _ in queries:
            header = read_exact(sock, 4)
            read_exact(sock, int.from_bytes(header, "big"))
        elapsed = time.perf_counter() - started
        sock.close()
        return elapsed
    finally:
        process.terminate()
        process.wait(timeout=10)


def read_exact(sock, size):
    result = bytearray()
    while len(result) < size:
        chunk = sock.recv(size - len(result))
        if not chunk:
            raise RuntimeError("connection closed")
        result.extend(chunk)
    return result


def sqlite_run():
    db = sqlite3.connect(":memory:")
    started = time.perf_counter()
    db.execute("CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT)")
    for i in range(ROWS):
        db.execute("INSERT INTO users VALUES (?, ?)", (i, f"user-{i}"))
    for i in range(SELECTS):
        db.execute("SELECT * FROM users WHERE id = ?", (i,)).fetchone()
    db.commit()
    return time.perf_counter() - started


if __name__ == "__main__":
    print(f"workload: {ROWS} inserts + {SELECTS} primary-key selects")
    print(f"sqlite: {sqlite3.sqlite_version}")
    superdb = superdb_run()
    sqlite = sqlite_run()
    print(f"superdb: {superdb:.3f}s ({(ROWS + SELECTS) / superdb:.0f} ops/s, TCP)")
    print(f"sqlite:  {sqlite:.3f}s ({(ROWS + SELECTS) / sqlite:.0f} ops/s, Python API)")
    print(f"ratio:   SuperDB took {superdb / sqlite:.2f}x SQLite's time")
