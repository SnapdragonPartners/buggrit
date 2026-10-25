**P1 · correctness** · confidence: high — Nil map write when cache is cold

`New` leaves `c.entries` nil, so the first `Put` panics.

**Impact:** Any request before warm-up crashes the worker.

**Fix:** Initialize `c.entries` in `New`.

```suggestion
func (c *Cache) Put(k string, v int) {
	if c.entries == nil {
		c.entries = map[string]int{}
	}
	c.entries[k] = v
}
```

Also applies to: `lru.go:88`, `ttl.go` (same class: *uninitialized map on constructor path*).

<sub>buggrit f-7f3a2c · blocking · To fix it, push the fix and leave this thread open: buggrit checks on the next push and resolves it. Not fixing it? Resolve the thread; that settles it, and buggrit won't raise it again.</sub>
<!-- buggrit:v1 {"id":"f-7f3a2c","fp":"7f3a2c0011223344","sev":"P1","sha":"1111111111111111111111111111111111111111","pass":2} -->