#include <stdio.h>
#include "superdb.h"

int main(void) {
    superdb_t *db = superdb_open("./hello.sdb", NULL);
    char *v;
    if (!db) { printf("open failed\n"); return 1; }
    superdb_set_str(db, "greeting", "hello, local-first!");
    v = superdb_get_str(db, "greeting");
    printf("greeting = %s\n", v ? v : "(nil)");
    superdb_free(v);
    printf("keys in db: %lu\n", (unsigned long)superdb_count(db));
    superdb_close(db);
    return 0;
}
