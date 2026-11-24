/* sdb_thread — implementation. See sdb_thread.h. */
#if !defined(_WIN32) && !defined(_POSIX_C_SOURCE)
#  define _POSIX_C_SOURCE 200809L
#endif
#include "superdb_thread.h"

#include <stdint.h>
#include <stdlib.h>

#ifdef _WIN32
#  include <process.h>

typedef struct { sdb_thread_fn fn; void *arg; } sdb_tramp_t;

static unsigned __stdcall sdb_tramp(void *p) {
    sdb_tramp_t t = *(sdb_tramp_t*)p;
    free(p);
    t.fn(t.arg);
    return 0;
}

int sdb_mutex_init(sdb_mutex_t *m) { InitializeCriticalSection(m); return 0; }
void sdb_mutex_destroy(sdb_mutex_t *m) { DeleteCriticalSection(m); }

int sdb_thread_start(sdb_thread_t *t, sdb_thread_fn fn, void *arg) {
    sdb_tramp_t *p = (sdb_tramp_t*)malloc(sizeof *p);
    uintptr_t h;
    if (!p) return -1;
    p->fn = fn; p->arg = arg;
    h = _beginthreadex(NULL, 0, sdb_tramp, p, 0, NULL);
    if (!h) { free(p); return -1; }
    *t = (HANDLE)h;
    return 0;
}

void sdb_thread_join(sdb_thread_t *t) {
    if (!t || !*t) return;
    WaitForSingleObject(*t, INFINITE);
    CloseHandle(*t);
    *t = NULL;
}

void sdb_sleep_ms(unsigned ms) { Sleep(ms); }

#else
#  include <unistd.h>
#  include <sched.h>
#  include <time.h>

typedef struct { sdb_thread_fn fn; void *arg; } sdb_tramp_t;

static void *sdb_tramp(void *p) {
    sdb_tramp_t t = *(sdb_tramp_t*)p;
    free(p);
    t.fn(t.arg);
    return NULL;
}

int sdb_mutex_init(sdb_mutex_t *m) { return pthread_mutex_init(m, NULL); }
void sdb_mutex_destroy(sdb_mutex_t *m) { pthread_mutex_destroy(m); }

int sdb_thread_start(sdb_thread_t *t, sdb_thread_fn fn, void *arg) {
    sdb_tramp_t *p = (sdb_tramp_t*)malloc(sizeof *p);
    if (!p) return -1;
    p->fn = fn; p->arg = arg;
    if (pthread_create(t, NULL, sdb_tramp, p) != 0) { free(p); return -1; }
    return 0;
}

void sdb_thread_join(sdb_thread_t *t) {
    if (t) pthread_join(*t, NULL);
}

void sdb_sleep_ms(unsigned ms) {
    struct timespec ts;
    ts.tv_sec = ms / 1000;
    ts.tv_nsec = (long)(ms % 1000) * 1000000L;
    nanosleep(&ts, NULL);
}
#endif
