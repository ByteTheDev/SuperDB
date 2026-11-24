#include <stdio.h>
#include "superdb.h"

/* Bulk-load 100k keys with a single fsync at the end. */
int main(void) {
    superdb_t *db = superdb_open("./batch.sdb", NULL);
    char key[64];
    int i;
    if (!db) { printf("open failed\n"); return 1; }
    superdb_batch_begin(db);
    for (i = 0; i < 100000; i++) {
        snprintf(key, sizeof key, "key:%08d", i);
        if (superdb_set_str(db, key, "value") != 0) { printf("set failed\n"); return 1; }
    }
    superdb_batch_end(db); /* one fsync for all 100k */
    printf("wrote %lu keys\n", (unsigned long)superdb_count(db));
    superdb_close(db);
    return 0;
}
