/* sdb_crc — implementation. See sdb_crc.h. */
#include "sdb_crc.h"

static uint32_t sdb_crc_tab[256];
static int sdb_crc_tab_ok = 0;

void sdb_crc_init(void) {
    uint32_t i, j, c;
    if (sdb_crc_tab_ok) return;
    for (i = 0; i < 256; i++) {
        c = i;
        for (j = 0; j < 8; j++)
            c = (c & 1) ? (0xEDB88320u ^ (c >> 1)) : (c >> 1);
        sdb_crc_tab[i] = c;
    }
    sdb_crc_tab_ok = 1;
}

uint32_t sdb_crc_up(uint32_t s, const unsigned char *p, size_t n) {
    size_t i;
    for (i = 0; i < n; i++)
        s = sdb_crc_tab[(s ^ p[i]) & 0xFF] ^ (s >> 8);
    return s;
}

uint32_t sdb_crc32(const unsigned char *p, size_t n) {
    sdb_crc_init();
    return sdb_crc_up(0xFFFFFFFFu, p, n) ^ 0xFFFFFFFFu;
}
