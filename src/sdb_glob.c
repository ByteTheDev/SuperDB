/* sdb_glob — implementation. See sdb_glob.h. */
#include "sdb_glob.h"

static int glob_rec(const unsigned char *p, const unsigned char *pe,
                    const unsigned char *s, const unsigned char *se) {
    while (p < pe) {
        switch (*p) {
        case '*': {
            while (p < pe && *p == '*') p++;
            if (p == pe) return 1;
            for (; s <= se; s++)
                if (glob_rec(p, pe, s, se)) return 1;
            return 0;
        }
        case '?':
            if (s >= se) return 0;
            p++; s++;
            break;
        case '[': {
            const unsigned char *q = p + 1;
            int neg = 0, ok = 0;
            if (s >= se) return 0;
            if (q < pe && *q == '^') { neg = 1; q++; }
            while (q < pe && *q != ']') {
                if (q + 2 < pe && q[1] == '-' && q[2] != ']') {
                    if (*s >= q[0] && *s <= q[2]) ok = 1;
                    q += 3;
                } else {
                    if (*s == *q) ok = 1;
                    q++;
                }
            }
            if (q >= pe) return 0;          /* unterminated class: no match */
            if (ok == neg) return 0;
            p = q + 1; s++;
            break;
        }
        case '\\':
            if (p + 1 < pe) p++;            /* escaped char is literal */
            /* fall through */
        default:
            if (s >= se || *s != *p) return 0;
            p++; s++;
            break;
        }
    }
    return s == se;
}

int sdb_glob_match(const char *pat, size_t plen, const char *str, size_t slen) {
    return glob_rec((const unsigned char*)pat, (const unsigned char*)pat + plen,
                    (const unsigned char*)str, (const unsigned char*)str + slen);
}
