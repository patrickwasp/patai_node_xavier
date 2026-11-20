"""patai: SQLite's temporary files in memory.

WASI's Python answers no to os.access() for every folder, so SQLite finds none to write its temporary files to, and
a sort, GROUP BY or index bigger than its cache fails with "disk I/O error". Imported when Eryx's runtime is
pre-initialized, so every connection keeps them in the guest's memory instead.
"""

import sqlite3
import sqlite3.dbapi2

_connect = sqlite3.connect


def connect(*args, **kwargs):
    connection = _connect(*args, **kwargs)
    connection.execute("PRAGMA temp_store = MEMORY")
    return connection


connect.__doc__ = _connect.__doc__
sqlite3.connect = sqlite3.dbapi2.connect = connect
