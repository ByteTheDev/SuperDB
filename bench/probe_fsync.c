/* probe_fsync — isolate stdio buffer size effect on fflush+_commit cost (Windows). */
#include <stdio.h>
#include <string.h>
#ifdef _WIN32
#  include <windows.h>
#  include <io.h>
static double now_s(void) {
    LARGE_INTEGER f, c;
    QueryPerformanceFrequency(&f); QueryPerformanceCounter(&c);
    return (double)c.QuadPart / (double)f.QuadPart;
}
#else
#  include <unistd.h>
#  include <sys/time.h>
static double now_s(void) {
    struct timeval tv; gettimeofday(&tv, NULL);
    return tv.tv_sec + tv.tv_usec / 1e6;
}
#endif

static void run(const char *path, size_t bufsz, int single_write) {
    FILE *f = fopen(path, "w+b");
    char rec[140];
    int i;
    double t0, t1;
    memset(rec, 'x', sizeof rec);
    if (bufsz) setvbuf(f, NULL, _IOFBF, bufsz);
    t0 = now_s();
    for (i = 0; i < 3000; i++) {
        if (single_write) {
            fwrite(rec, 1, sizeof rec, f);
        } else {
            fwrite(rec, 1, 4, f);
            fwrite(rec + 4, 1, sizeof(rec) - 4, f);
        }
        fflush(f);
#ifdef _WIN32
        _commit(_fileno(f));
#else
        fsync(fileno(f));
#endif
    }
    t1 = now_s();
    fclose(f);
    printf("buf=%7lu single=%d : 3000 ops in %.3fs = %.0f ops/sec\n",
           (unsigned long)bufsz, single_write, t1 - t0, 3000 / (t1 - t0));
    remove(path);
}

int main(void) {
    run("./probe_a.bin", 0, 0);          /* default buffer, 2 writes (v1 style) */
    run("./probe_a.bin", 0, 1);          /* default buffer, 1 write */
    run("./probe_a.bin", 4096, 1);
    run("./probe_a.bin", 1u << 20, 1);   /* 1MB buffer (v2 style) */
    run("./probe_a.bin", 1u << 20, 0);
    return 0;
}
