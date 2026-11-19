# filehash

`import "github.com/iotku/CalibreFast/pkg/filehash"`

SHA-256 of files, cached by path, size and modification time, so asking
again about an unchanged file doesn't read it (or touch the disk beyond
the caller's `os.Stat`).

```go
info, err := os.Stat(p)
sum, err := filehash.SHA256(p, info) // hex; default cache of 50000 entries

c := filehash.NewCache(100) // or a cache of your own
sum, err = c.SHA256(p, info)
```

## Limits

- A file rewritten with the same size and modification time (to the
  nanosecond, as the file system reports it) gets its old hash.
- When the cache is full the least recently used hash is dropped. A
  changed file replaces its own entry.
