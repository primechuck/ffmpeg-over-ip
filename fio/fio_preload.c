/* LD_PRELOAD shim — transparent file I/O over TCP without patching ffmpeg.
 * Build: gcc -fPIC -shared -o libfio_preload.so fio/fio.c fio/fio_preload.c -Ifio -ldl -lpthread
 * Use: FFOIP_PORT=5000 LD_PRELOAD=./libfio_preload.so ffmpeg -i /media/a.mkv ...
 *
 * This wraps open/read/write/lseek/close/fstat/ftruncate/unlink/rename/mkdir
 * and routes them through fio_* which does passthrough when FFOIP_PORT unset,
 * and tunnels + optional short-circuit when set. Same logic as patched file.c
 * but works with stock jellyfin-ffmpeg binary.
 */

#define _GNU_SOURCE
#include <dlfcn.h>
#include <stdarg.h>
#include <fcntl.h>
#include <unistd.h>
#include <sys/stat.h>
#include <sys/types.h>
#include <errno.h>

#include "fio.h"

/* Real syscall pointers */
static int (*real_open)(const char *, int, ...) = NULL;
static int (*real_open64)(const char *, int, ...) = NULL;
static int (*real_openat)(int, const char *, int, ...) = NULL;
static int (*real_openat64)(int, const char *, int, ...) = NULL;
static ssize_t (*real_read)(int, void *, size_t) = NULL;
static ssize_t (*real_write)(int, const void *, size_t) = NULL;
static off_t (*real_lseek)(int, off_t, int) = NULL;
static int (*real_close)(int) = NULL;
static int (*real_fstat)(int, struct stat *) = NULL;
static int (*real___fxstat)(int, int, struct stat *) = NULL;
static int (*real_ftruncate)(int, off_t) = NULL;
static int (*real_unlink)(const char *) = NULL;
static int (*real_rename)(const char *, const char *) = NULL;
static int (*real_mkdir)(const char *, mode_t) = NULL;
static int (*real_access)(const char *, int) = NULL;
static int (*real_stat)(const char *, struct stat *) = NULL;
static int (*real___xstat)(int, const char *, struct stat *) = NULL;

static void ensure_real(void) {
    if (real_open) return;
    real_open = dlsym(RTLD_NEXT, "open");
    real_open64 = dlsym(RTLD_NEXT, "open64");
    real_openat = dlsym(RTLD_NEXT, "openat");
    real_openat64 = dlsym(RTLD_NEXT, "openat64");
    real_read = dlsym(RTLD_NEXT, "read");
    real_write = dlsym(RTLD_NEXT, "write");
    real_lseek = dlsym(RTLD_NEXT, "lseek");
    real_close = dlsym(RTLD_NEXT, "close");
    real_fstat = dlsym(RTLD_NEXT, "fstat");
    real___fxstat = dlsym(RTLD_NEXT, "__fxstat");
    real_ftruncate = dlsym(RTLD_NEXT, "ftruncate");
    real_unlink = dlsym(RTLD_NEXT, "unlink");
    real_rename = dlsym(RTLD_NEXT, "rename");
    real_mkdir = dlsym(RTLD_NEXT, "mkdir");
    real_access = dlsym(RTLD_NEXT, "access");
    real_stat = dlsym(RTLD_NEXT, "stat");
    real___xstat = dlsym(RTLD_NEXT, "__xstat");
}

/* open with varargs mode */
int open(const char *path, int flags, ...) {
    ensure_real();
    mode_t mode = 0;
    if (flags & O_CREAT) {
        va_list ap;
        va_start(ap, flags);
        mode = (mode_t)va_arg(ap, int);
        va_end(ap);
    }
    return fio_open(path, flags, mode);
}

int open64(const char *path, int flags, ...) {
    ensure_real();
    mode_t mode = 0;
    if (flags & O_CREAT) {
        va_list ap;
        va_start(ap, flags);
        mode = (mode_t)va_arg(ap, int);
        va_end(ap);
    }
    return fio_open(path, flags, mode);
}

int openat(int dirfd, const char *path, int flags, ...) {
    ensure_real();
    if (path && path[0] == '/') {
        mode_t mode = 0;
        if (flags & O_CREAT) {
            va_list ap;
            va_start(ap, flags);
            mode = (mode_t)va_arg(ap, int);
            va_end(ap);
        }
        return fio_open(path, flags, mode);
    }
    mode_t mode = 0;
    if (flags & O_CREAT) {
        va_list ap;
        va_start(ap, flags);
        mode = (mode_t)va_arg(ap, int);
        va_end(ap);
        return real_openat(dirfd, path, flags, mode);
    }
    return real_openat(dirfd, path, flags);
}

int openat64(int dirfd, const char *path, int flags, ...) {
    ensure_real();
    if (path && path[0] == '/') {
        mode_t mode = 0;
        if (flags & O_CREAT) {
            va_list ap;
            va_start(ap, flags);
            mode = (mode_t)va_arg(ap, int);
            va_end(ap);
        }
        return fio_open(path, flags, mode);
    }
    mode_t mode = 0;
    if (flags & O_CREAT) {
        va_list ap;
        va_start(ap, flags);
        mode = (mode_t)va_arg(ap, int);
        va_end(ap);
        return real_openat64(dirfd, path, flags, mode);
    }
    return real_openat64(dirfd, path, flags);
}

ssize_t read(int fd, void *buf, size_t count) {
    ensure_real();
    ssize_t r = fio_read(fd, buf, count);
    return r;
}

ssize_t write(int fd, const void *buf, size_t count) {
    ensure_real();
    return fio_write(fd, buf, count);
}

off_t lseek(int fd, off_t offset, int whence) {
    ensure_real();
    return fio_lseek(fd, offset, whence);
}

int close(int fd) {
    ensure_real();
    return fio_close(fd);
}

int fstat(int fd, struct stat *st) {
    ensure_real();
    return fio_fstat(fd, st);
}

int __fxstat(int ver, int fd, struct stat *st) {
    ensure_real();
    (void)ver;
    return fio_fstat(fd, st);
}

int ftruncate(int fd, off_t length) {
    ensure_real();
    return fio_ftruncate(fd, length);
}

int unlink(const char *path) {
    ensure_real();
    return fio_unlink(path);
}

int rename(const char *oldpath, const char *newpath) {
    ensure_real();
    return fio_rename(oldpath, newpath);
}

int mkdir(const char *path, mode_t mode) {
    ensure_real();
    return fio_mkdir(path, mode);
}

/* Keep access/stat as real to avoid breaking file_check that uses access */
int access(const char *path, int mode) {
    ensure_real();
    /* Let file_check use real access - it only checks existence, not content */
    return real_access ? real_access(path, mode) : -1;
}
