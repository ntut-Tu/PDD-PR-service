// Package upstream owns provider request execution and response caching.
//
// Client selects fresh responses, deduplicates calls, revalidates ETags and
// chooses stale fallback. Its private executor owns HTTP I/O, quota tracking
// and bounded retries. These are cooperating implementations in one package;
// they do not require service or strategy interfaces.
//
// ResponseCache is the storage capability needed by Client. MemoryStore and
// CacheStore implement it for memory and SQLite respectively. CacheStore also
// owns expiry cleanup. Application settings are translated at the gateway's
// composition root rather than inside this component.
package upstream
