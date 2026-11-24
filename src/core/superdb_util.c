/* superdb_util - implementation. See superdb_util.h. */
#if !defined(_WIN32) && !defined(_POSIX_C_SOURCE)
#  define _POSIX_C_SOURCE 200809L
#endif
#include "superdb_util.h"
#include "superdb_int.h"

#include <stdlib.h>
#include <string.h>

#ifdef _WIN32
#  ifndef WIN32_LEAN_AND_MEAN
#  define WIN32_LEAN_AND_MEAN
#  endif
#  include <windows.h>
#  include <io.h>
#else
#  include <unistd.h>
#  include <sys/time.h>
#  include <time.h>
#endif

int sdb_buf_put(sdb_buf_t *b, const void *p, size_t n) {
    if (b->n + n > b->cap) {
        size_t nc = b->cap ? b->cap : 256;
        unsigned char *np;
        while (nc < b->n + n) nc *= 2;
        np = (unsigned char*)realloc(b->p, nc);
        if (!np) return -1;
        b->p = np; b->cap = nc;
    }
    if (n) memcpy(b->p + b->n, p, n);
    b->n += n;
    return 0;
}

void sdb_buf_free(sdb_buf_t *b) {
    free(b->p);
    b->p = NULL; b->n = 0; b->cap = 0;
}

int64_t sdb_now_ms(void) {
#ifdef _WIN32
    FILETIME ft;
    ULARGE_INTEGER u;
    GetSystemTimeAsFileTime(&ft);
    u.LowPart = ft.dwLowDateTime; u.HighPart = ft.dwHighDateTime;
    /* 100ns since 1601 -> ms since 1970 */
    return (int64_t)(u.QuadPart / 10000ULL) - (int64_t)11644473600000ULL;
#else
    struct timeval tv;
    gettimeofday(&tv, NULL);
    return (int64_t)tv.tv_sec * 1000 + tv.tv_usec / 1000;
#endif
}

int64_t sdb_now_us(void) {
#ifdef _WIN32
    static LARGE_INTEGER freq;  /* set once; benign race (same value) */
    LARGE_INTEGER c;
    if (!freq.QuadPart) QueryPerformanceFrequency(&freq);
    QueryPerformanceCounter(&c);
    return (int64_t)(c.QuadPart * 1000000LL / freq.QuadPart);
#else
    struct timespec ts;
    clock_gettime(CLOCK_MONOTONIC, &ts);
    return (int64_t)ts.tv_sec * 1000000 + ts.tv_nsec / 1000;
#endif
}

uint64_t sdb_rand64(uint64_t *state) {
    uint64_t z = (*state += 0x9E3779B97F4A7C15ULL);
    z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9ULL;
    z = (z ^ (z >> 27)) * 0x94D049BB133111EBULL;
    return z ^ (z >> 31);
}

void sdb_put32le(unsigned char *b, uint32_t v) {
    b[0]=(unsigned char)v; b[1]=(unsigned char)(v>>8);
    b[2]=(unsigned char)(v>>16); b[3]=(unsigned char)(v>>24);
}

void sdb_put64le(unsigned char *b, uint64_t v) {
    int i; for (i = 0; i < 8; i++) b[i] = (unsigned char)(v >> (8*i));
}

uint32_t sdb_get32le(const unsigned char *b) {
    return (uint32_t)b[0] | ((uint32_t)b[1]<<8) |
           ((uint32_t)b[2]<<16) | ((uint32_t)b[3]<<24);
}

uint64_t sdb_get64le(const unsigned char *b) {
    uint64_t v = 0; int i; for (i = 0; i < 8; i++) v |= ((uint64_t)b[i]) << (8*i);
    return v;
}

uint64_t sdb_hash_len(const char *s, size_t n) { /* FNV-1a 64 */
    uint64_t h = 14695981039346656037ULL;
    size_t i;
    for (i = 0; i < n; i++) { h ^= (unsigned char)s[i]; h *= 1099511628211ULL; }
    return h;
}

size_t sdb_pow2(size_t n) {
    size_t p = 16;
    while (p < n) p <<= 1;
    return p;
}

void sdb_fsync(FILE *f) {
    if (!f) return;
    fflush(f);
#ifdef _WIN32
    _commit(_fileno(f));
#else
    fsync(fileno(f));
#endif
}

void sdb_buf_log(FILE *f) {
    if (f) setvbuf(f, NULL, _IOFBF, SDB_LOG_BUF);
}

int sdb_truncate(FILE *f, long len) {
    if (!f) return -1;
    fflush(f);
#ifdef _WIN32
    return _chsize(_fileno(f), len);
#else
    return ftruncate(fileno(f), (off_t)len);
#endif
}

int sdb_replace(const char *tmp, const char *dst) {
#ifdef _WIN32
    /* rename() fails when dst exists on Windows; MoveFileEx replaces. */
    if (MoveFileExA(tmp, dst, MOVEFILE_REPLACE_EXISTING | MOVEFILE_WRITE_THROUGH))
        return 0;
    remove(dst);
    return rename(tmp, dst);
#else
    /* POSIX rename() atomically replaces dst. */
    return rename(tmp, dst);
#endif
}

void sdb_set_err(superdb_t *db, const char *m) {
    if (db) {
        strncpy(db->errmsg, m, sizeof(db->errmsg) - 1);
        db->errmsg[sizeof(db->errmsg) - 1] = '\0';
    }
}
