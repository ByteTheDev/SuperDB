/* sdb_glob — Redis-style glob matching: * ? [abc] [a-z] [^x] and \ escapes.
 * Pure function, no state. Private to SuperDB core. */
#ifndef SDB_GLOB_H
#define SDB_GLOB_H

#include <stddef.h>

/* 1 = str matches pattern, 0 = no match. Bytes are compared unsigned. */
int sdb_glob_match(const char *pat, size_t plen, const char *str, size_t slen);

#endif
