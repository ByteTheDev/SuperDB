/* sdb_crc — CRC32 (IEEE) with table-driven fast path + streaming update.
 * Same digests as the original bitwise version. Private to SuperDB core. */
#ifndef SDB_CRC_H
#define SDB_CRC_H

#include <stddef.h>
#include <stdint.h>

void     sdb_crc_init(void);
/* Streaming: state starts at 0xFFFFFFFFu, xor with 0xFFFFFFFFu at end. */
uint32_t sdb_crc_up(uint32_t state, const unsigned char *p, size_t n);
uint32_t sdb_crc32(const unsigned char *p, size_t n);

#endif
