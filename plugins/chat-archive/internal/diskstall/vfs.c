// A pass-through SQLite VFS that asks Go (diskstallGate) before each read, write, sync and
// truncate, so a test can stall or fail the outbox's disk I/O. It wraps the process's default VFS
// (unix) and registers itself as the default, so every database opened afterwards by
// mattn/go-sqlite3 in the test binary goes through it. The sqlite3 symbols resolve against the
// amalgamation mattn/go-sqlite3 links into the same binary.

#include <stdlib.h>
#include <string.h>
#include "vfs.h"
#include "_cgo_export.h"

typedef struct {
	sqlite3_file base; // pMethods = &shim_methods
	int id;            // Go's handle for the file (its path and kind)
	sqlite3_file *real; // the wrapped VFS's file, allocated right after this struct
} shim_file;

static sqlite3_vfs *real_vfs;
static sqlite3_vfs shim_vfs;
static sqlite3_io_methods shim_methods_v1, shim_methods_v2, shim_methods_v3;

#define REAL(f) (((shim_file *)(f))->real)
#define ID(f) (((shim_file *)(f))->id)

static int shim_close(sqlite3_file *f) {
	int rc = SQLITE_OK;
	if (REAL(f)->pMethods) {
		rc = REAL(f)->pMethods->xClose(REAL(f));
	}
	diskstallClose(ID(f));
	return rc;
}

static int shim_read(sqlite3_file *f, void *p, int n, sqlite3_int64 off) {
	int rc = diskstallGate(ID(f), DISKSTALL_READ);
	if (rc != SQLITE_OK) {
		return rc;
	}
	return REAL(f)->pMethods->xRead(REAL(f), p, n, off);
}

static int shim_write(sqlite3_file *f, const void *p, int n, sqlite3_int64 off) {
	int rc = diskstallGate(ID(f), DISKSTALL_WRITE);
	if (rc != SQLITE_OK) {
		return rc;
	}
	return REAL(f)->pMethods->xWrite(REAL(f), p, n, off);
}

static int shim_truncate(sqlite3_file *f, sqlite3_int64 size) {
	int rc = diskstallGate(ID(f), DISKSTALL_TRUNCATE);
	if (rc != SQLITE_OK) {
		return rc;
	}
	return REAL(f)->pMethods->xTruncate(REAL(f), size);
}

static int shim_sync(sqlite3_file *f, int flags) {
	int rc = diskstallGate(ID(f), DISKSTALL_SYNC);
	if (rc != SQLITE_OK) {
		return rc;
	}
	return REAL(f)->pMethods->xSync(REAL(f), flags);
}

static int shim_file_size(sqlite3_file *f, sqlite3_int64 *size) {
	return REAL(f)->pMethods->xFileSize(REAL(f), size);
}
static int shim_lock(sqlite3_file *f, int l) { return REAL(f)->pMethods->xLock(REAL(f), l); }
static int shim_unlock(sqlite3_file *f, int l) { return REAL(f)->pMethods->xUnlock(REAL(f), l); }
static int shim_check_reserved(sqlite3_file *f, int *out) {
	return REAL(f)->pMethods->xCheckReservedLock(REAL(f), out);
}
static int shim_file_control(sqlite3_file *f, int op, void *arg) {
	return REAL(f)->pMethods->xFileControl(REAL(f), op, arg);
}
static int shim_sector_size(sqlite3_file *f) { return REAL(f)->pMethods->xSectorSize(REAL(f)); }
static int shim_device(sqlite3_file *f) { return REAL(f)->pMethods->xDeviceCharacteristics(REAL(f)); }
static int shim_shm_map(sqlite3_file *f, int pg, int sz, int extend, void volatile **pp) {
	return REAL(f)->pMethods->xShmMap(REAL(f), pg, sz, extend, pp);
}
static int shim_shm_lock(sqlite3_file *f, int off, int n, int flags) {
	return REAL(f)->pMethods->xShmLock(REAL(f), off, n, flags);
}
static void shim_shm_barrier(sqlite3_file *f) { REAL(f)->pMethods->xShmBarrier(REAL(f)); }
static int shim_shm_unmap(sqlite3_file *f, int del) { return REAL(f)->pMethods->xShmUnmap(REAL(f), del); }
static int shim_fetch(sqlite3_file *f, sqlite3_int64 off, int n, void **pp) {
	return REAL(f)->pMethods->xFetch(REAL(f), off, n, pp);
}
static int shim_unfetch(sqlite3_file *f, sqlite3_int64 off, void *p) {
	return REAL(f)->pMethods->xUnfetch(REAL(f), off, p);
}

static void fill_methods(sqlite3_io_methods *m, int version) {
	memset(m, 0, sizeof(*m));
	m->iVersion = version;
	m->xClose = shim_close;
	m->xRead = shim_read;
	m->xWrite = shim_write;
	m->xTruncate = shim_truncate;
	m->xSync = shim_sync;
	m->xFileSize = shim_file_size;
	m->xLock = shim_lock;
	m->xUnlock = shim_unlock;
	m->xCheckReservedLock = shim_check_reserved;
	m->xFileControl = shim_file_control;
	m->xSectorSize = shim_sector_size;
	m->xDeviceCharacteristics = shim_device;
	if (version >= 2) {
		m->xShmMap = shim_shm_map;
		m->xShmLock = shim_shm_lock;
		m->xShmBarrier = shim_shm_barrier;
		m->xShmUnmap = shim_shm_unmap;
	}
	if (version >= 3) {
		m->xFetch = shim_fetch;
		m->xUnfetch = shim_unfetch;
	}
}

static int file_kind(int flags) {
	if (flags & SQLITE_OPEN_MAIN_DB) {
		return DISKSTALL_MAIN_DB;
	}
	if (flags & SQLITE_OPEN_WAL) {
		return DISKSTALL_WAL;
	}
	if (flags & SQLITE_OPEN_MAIN_JOURNAL) {
		return DISKSTALL_JOURNAL;
	}
	return DISKSTALL_OTHER;
}

static int shim_open(sqlite3_vfs *vfs, const char *name, sqlite3_file *f, int flags, int *out) {
	shim_file *s = (shim_file *)f;
	(void)vfs;
	s->real = (sqlite3_file *)(s + 1);
	s->base.pMethods = 0;
	int rc = real_vfs->xOpen(real_vfs, name, s->real, flags, out);
	if (rc != SQLITE_OK || s->real->pMethods == 0) {
		return rc;
	}
	s->id = diskstallOpen((char *)(name ? name : ""), file_kind(flags));
	switch (s->real->pMethods->iVersion) {
	case 1:
		s->base.pMethods = &shim_methods_v1;
		break;
	case 2:
		s->base.pMethods = &shim_methods_v2;
		break;
	default:
		s->base.pMethods = &shim_methods_v3;
		break;
	}
	return rc;
}

int diskstall_register(void) {
	if (real_vfs) {
		return SQLITE_OK;
	}
	sqlite3_vfs *v = sqlite3_vfs_find(0);
	if (v == 0) {
		return SQLITE_ERROR;
	}
	fill_methods(&shim_methods_v1, 1);
	fill_methods(&shim_methods_v2, 2);
	fill_methods(&shim_methods_v3, 3);
	shim_vfs = *v; // every other method passes straight to the wrapped VFS
	shim_vfs.pNext = 0;
	shim_vfs.zName = "diskstall";
	shim_vfs.szOsFile = (int)sizeof(shim_file) + v->szOsFile;
	shim_vfs.xOpen = shim_open;
	real_vfs = v;
	return sqlite3_vfs_register(&shim_vfs, 1);
}
