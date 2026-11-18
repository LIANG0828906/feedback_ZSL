# SQLite 3.42.0 native assets

The project pins SQLite `3.42.0` because the operational baseline requires that
native library version. The NuGet `SQLitePCLRaw.bundle_e_sqlite3` feed jumps
from `3.41.2` (package 2.1.6) to `3.44.0` (package 2.1.7), so the native assets
in this directory are used to replace the package-provided binaries after build
and publish.

- `linux-x64/libe_sqlite3.so`: compiled from the official
  `sqlite-amalgamation-3420000.zip`.
- `win-x64/e_sqlite3.dll`: official
  `sqlite-dll-win64-x64-3420000.zip`, renamed to the SQLitePCLRaw native name.
- `win-x86/e_sqlite3.dll`: official
  `sqlite-dll-win32-x86-3420000.zip`, renamed to the SQLitePCLRaw native name.

SQLite is in the public domain:
https://www.sqlite.org/copyright.html

The Linux library was compiled with:

```bash
gcc -shared -fPIC -O2 \
  -DSQLITE_THREADSAFE=1 \
  -DSQLITE_ENABLE_COLUMN_METADATA=1 \
  -DSQLITE_ENABLE_FTS5 \
  -DSQLITE_ENABLE_JSON1 \
  sqlite3.c -o libe_sqlite3.so -lpthread -ldl -lm
```
