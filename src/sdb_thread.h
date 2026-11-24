/* sdb_thread — mutex + thread + sleep, over pthreads (POSIX) or Win32.
 * Private to SuperDB. Mutexes are NOT recursive on either platform. */
#ifndef SDB_THREAD_H
#define SDB_THREAD_H

#ifdef _WIN32
#  ifndef WIN32_LEAN_AND_MEAN
#  define WIN32_LEAN_AND_MEAN
#  endif
#  include <windows.h>
typedef CRITICAL_SECTION sdb_mutex_t;
typedef HANDLE sdb_thread_t;
#else
#  include <pthread.h>
typedef pthread_mutex_t sdb_mutex_t;
typedef pthread_t sdb_thread_t;
#endif

typedef void (*sdb_thread_fn)(void *arg);

int  sdb_mutex_init(sdb_mutex_t *m);     /* 0 = ok */
void sdb_mutex_destroy(sdb_mutex_t *m);

#ifdef _WIN32
static inline void sdb_mutex_lock(sdb_mutex_t *m)   { EnterCriticalSection(m); }
static inline void sdb_mutex_unlock(sdb_mutex_t *m) { LeaveCriticalSection(m); }
#else
static inline void sdb_mutex_lock(sdb_mutex_t *m)   { pthread_mutex_lock(m); }
static inline void sdb_mutex_unlock(sdb_mutex_t *m) { pthread_mutex_unlock(m); }
#endif

/* Starts a detached-by-join thread running fn(arg). 0 = ok. Must be joined. */
int  sdb_thread_start(sdb_thread_t *t, sdb_thread_fn fn, void *arg);
void sdb_thread_join(sdb_thread_t *t);
void sdb_sleep_ms(unsigned ms);

#endif
