// The part of SQLite's public VFS interface (sqlite3.h, stable since 3.7) that the shim uses. The
// layouts match sqlite3_file, sqlite3_io_methods (version 3) and sqlite3_vfs (version 3).

#ifndef DISKSTALL_VFS_H
#define DISKSTALL_VFS_H

typedef long long sqlite3_int64;

#define SQLITE_OK 0
#define SQLITE_ERROR 1

#define SQLITE_OPEN_MAIN_DB 0x00000100
#define SQLITE_OPEN_MAIN_JOURNAL 0x00000800
#define SQLITE_OPEN_WAL 0x00080000

typedef struct sqlite3_io_methods sqlite3_io_methods;
typedef struct sqlite3_file sqlite3_file;
typedef struct sqlite3_vfs sqlite3_vfs;

struct sqlite3_file {
	const sqlite3_io_methods *pMethods;
};

struct sqlite3_io_methods {
	int iVersion;
	int (*xClose)(sqlite3_file *);
	int (*xRead)(sqlite3_file *, void *, int, sqlite3_int64);
	int (*xWrite)(sqlite3_file *, const void *, int, sqlite3_int64);
	int (*xTruncate)(sqlite3_file *, sqlite3_int64);
	int (*xSync)(sqlite3_file *, int);
	int (*xFileSize)(sqlite3_file *, sqlite3_int64 *);
	int (*xLock)(sqlite3_file *, int);
	int (*xUnlock)(sqlite3_file *, int);
	int (*xCheckReservedLock)(sqlite3_file *, int *);
	int (*xFileControl)(sqlite3_file *, int, void *);
	int (*xSectorSize)(sqlite3_file *);
	int (*xDeviceCharacteristics)(sqlite3_file *);
	int (*xShmMap)(sqlite3_file *, int, int, int, void volatile **);
	int (*xShmLock)(sqlite3_file *, int, int, int);
	void (*xShmBarrier)(sqlite3_file *);
	int (*xShmUnmap)(sqlite3_file *, int);
	int (*xFetch)(sqlite3_file *, sqlite3_int64, int, void **);
	int (*xUnfetch)(sqlite3_file *, sqlite3_int64, void *);
};

struct sqlite3_vfs {
	int iVersion;
	int szOsFile;
	int mxPathname;
	sqlite3_vfs *pNext;
	const char *zName;
	void *pAppData;
	int (*xOpen)(sqlite3_vfs *, const char *, sqlite3_file *, int, int *);
	int (*xDelete)(sqlite3_vfs *, const char *, int);
	int (*xAccess)(sqlite3_vfs *, const char *, int, int *);
	int (*xFullPathname)(sqlite3_vfs *, const char *, int, char *);
	void *(*xDlOpen)(sqlite3_vfs *, const char *);
	void (*xDlError)(sqlite3_vfs *, int, char *);
	void (*(*xDlSym)(sqlite3_vfs *, void *, const char *))(void);
	void (*xDlClose)(sqlite3_vfs *, void *);
	int (*xRandomness)(sqlite3_vfs *, int, char *);
	int (*xSleep)(sqlite3_vfs *, int);
	int (*xCurrentTime)(sqlite3_vfs *, double *);
	int (*xGetLastError)(sqlite3_vfs *, int, char *);
	int (*xCurrentTimeInt64)(sqlite3_vfs *, sqlite3_int64 *);
	int (*xSetSystemCall)(sqlite3_vfs *, const char *, void (*)(void));
	void (*(*xGetSystemCall)(sqlite3_vfs *, const char *))(void);
	const char *(*xNextSystemCall)(sqlite3_vfs *, const char *);
};

sqlite3_vfs *sqlite3_vfs_find(const char *);
int sqlite3_vfs_register(sqlite3_vfs *, int);

// Operations and file kinds the gate sees (diskstall.go mirrors them).
#define DISKSTALL_READ 1
#define DISKSTALL_WRITE 2
#define DISKSTALL_SYNC 4
#define DISKSTALL_TRUNCATE 8

#define DISKSTALL_MAIN_DB 1
#define DISKSTALL_WAL 2
#define DISKSTALL_JOURNAL 4
#define DISKSTALL_OTHER 8

int diskstall_register(void);

#endif
