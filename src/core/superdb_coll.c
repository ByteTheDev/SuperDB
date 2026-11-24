/* sdb_coll — implementation. See sdb_coll.h. */
#include "superdb_coll.h"
#include "superdb_util.h"

#include <stdlib.h>
#include <string.h>

/* ---------------- list ---------------- */

sdb_list_t *sdb_list_new(void) {
    sdb_list_t *l = (sdb_list_t*)calloc(1, sizeof *l);
    if (!l) return NULL;
    l->cap = 8;
    l->a = (sdb_blob_t*)calloc(l->cap, sizeof *l->a);
    if (!l->a) { free(l); return NULL; }
    return l;
}

void sdb_list_free(sdb_list_t *l) {
    size_t i;
    if (!l) return;
    for (i = 0; i < l->len; i++)
        free(l->a[(l->head + i) & (l->cap - 1)].p);
    free(l->a);
    free(l);
}

static int list_grow(sdb_list_t *l) {
    size_t i, nc = l->cap * 2;
    sdb_blob_t *na = (sdb_blob_t*)calloc(nc, sizeof *na);
    if (!na) return -1;
    for (i = 0; i < l->len; i++)
        na[i] = l->a[(l->head + i) & (l->cap - 1)];
    free(l->a);
    l->a = na; l->cap = nc; l->head = 0;
    return 0;
}

int sdb_list_push(sdb_list_t *l, int front, const void *p, size_t n) {
    sdb_blob_t b;
    if (l->len == l->cap && list_grow(l) != 0) return -1;
    b.p = (char*)malloc(n ? n : 1);
    if (!b.p) return -1;
    if (n) memcpy(b.p, p, n);
    b.n = n;
    if (front) {
        l->head = (l->head + l->cap - 1) & (l->cap - 1);
        l->a[l->head] = b;
    } else {
        l->a[(l->head + l->len) & (l->cap - 1)] = b;
    }
    l->len++;
    l->bytes += n;
    return 0;
}

int sdb_list_pop(sdb_list_t *l, int front, sdb_blob_t *out) {
    size_t idx;
    if (!l->len) return 0;
    if (front) {
        idx = l->head;
        l->head = (l->head + 1) & (l->cap - 1);
    } else {
        idx = (l->head + l->len - 1) & (l->cap - 1);
    }
    *out = l->a[idx];
    l->a[idx].p = NULL;
    l->a[idx].n = 0;
    l->len--;
    l->bytes -= out->n;
    if (!l->len) l->head = 0;
    return 1;
}

const sdb_blob_t *sdb_list_at(const sdb_list_t *l, size_t i) {
    return &l->a[(l->head + i) & (l->cap - 1)];
}

/* ---------------- hash table ---------------- */

sdb_htab_t *sdb_ht_new(void) {
    sdb_htab_t *t = (sdb_htab_t*)calloc(1, sizeof *t);
    if (!t) return NULL;
    t->nb = 16;
    t->b = (sdb_fent_t**)calloc(t->nb, sizeof *t->b);
    if (!t->b) { free(t); return NULL; }
    return t;
}

void sdb_ht_free(sdb_htab_t *t) {
    size_t i;
    if (!t) return;
    for (i = 0; i < t->nb; i++) {
        sdb_fent_t *e = t->b[i];
        while (e) {
            sdb_fent_t *nx = e->next;
            free(e->f); free(e->v); free(e);
            e = nx;
        }
    }
    free(t->b);
    free(t);
}

sdb_fent_t *sdb_ht_find(const sdb_htab_t *t, const void *f, size_t fl) {
    uint64_t h = sdb_hash_len((const char*)f, fl);
    sdb_fent_t *e = t->b[(size_t)(h & (uint64_t)(t->nb - 1))];
    while (e) {
        if (e->h == h && e->fl == fl && memcmp(e->f, f, fl) == 0) return e;
        e = e->next;
    }
    return NULL;
}

static int ht_rehash(sdb_htab_t *t, size_t nb) {
    sdb_fent_t **nbk = (sdb_fent_t**)calloc(nb, sizeof *nbk);
    size_t i;
    if (!nbk) return -1;
    for (i = 0; i < t->nb; i++) {
        sdb_fent_t *e = t->b[i];
        while (e) {
            sdb_fent_t *nx = e->next;
            size_t b = (size_t)(e->h & (uint64_t)(nb - 1));
            e->next = nbk[b]; nbk[b] = e;
            e = nx;
        }
    }
    free(t->b);
    t->b = nbk;
    t->nb = nb;
    return 0;
}

int sdb_ht_put(sdb_htab_t *t, const void *f, size_t fl, const void *v, size_t vl) {
    sdb_fent_t *e = sdb_ht_find(t, f, fl);
    if (e) {
        char *nv = NULL;
        if (vl) {
            nv = (char*)malloc(vl);
            if (!nv) return -1;
            memcpy(nv, v, vl);
        }
        free(e->v);
        e->v = nv; t->bytes = t->bytes - e->vl + vl; e->vl = vl;
        return 0;
    }
    e = (sdb_fent_t*)calloc(1, sizeof *e);
    if (!e) return -1;
    e->f = (char*)malloc(fl ? fl : 1);
    if (!e->f) { free(e); return -1; }
    if (fl) memcpy(e->f, f, fl);
    e->fl = fl;
    if (vl) {
        e->v = (char*)malloc(vl);
        if (!e->v) { free(e->f); free(e); return -1; }
        memcpy(e->v, v, vl);
        e->vl = vl;
    }
    e->h = sdb_hash_len((const char*)f, fl);
    {
        size_t b = (size_t)(e->h & (uint64_t)(t->nb - 1));
        e->next = t->b[b];
        t->b[b] = e;
    }
    t->count++;
    t->bytes += fl + vl;
    if (t->count > t->nb * 3 / 4) ht_rehash(t, t->nb * 2);
    return 1;
}

int sdb_ht_del(sdb_htab_t *t, const void *f, size_t fl) {
    uint64_t h = sdb_hash_len((const char*)f, fl);
    size_t b = (size_t)(h & (uint64_t)(t->nb - 1));
    sdb_fent_t *e = t->b[b], *prev = NULL;
    while (e) {
        if (e->h == h && e->fl == fl && memcmp(e->f, f, fl) == 0) {
            if (prev) prev->next = e->next; else t->b[b] = e->next;
            t->count--;
            t->bytes -= e->fl + e->vl;
            free(e->f); free(e->v); free(e);
            return 1;
        }
        prev = e; e = e->next;
    }
    return 0;
}
