# Services

Go module `pdd/services` holding PDD add-on microservices.

| Path | Content |
| --- | --- |
| `cmd/<service>/` | Entry point of one executable service |
| `internal/gitgateway/` | Gateway components and application assembly |
| `images/<service>/Dockerfile` | Container image of one service |

## Gateway components

The gateway is organized by capabilities. There is no mandatory
Controller → Service → Repository chain. Files in a package share the same
component; splitting a file does not introduce another layer.

| Component | Responsibility |
| --- | --- |
| `internal/gitgateway` | Assemble components, translate configuration, serve and shut down HTTP, run cache cleanup |
| `api` | HTTP routes, caller authentication, project authorization and JSON responses |
| `pulls` | Merge project PR/MR results, report per-repository failures, limit patches and resolve a pull's head tree |
| `repos` | Read trees and files at a commit, detect binary content and apply UTF-8-safe file limits |
| `github`, `gitlab` | Provider-specific queries, batching, pagination and mapping |
| `provider` | Normalized adapter result types and common REST/GraphQL decoding helpers; no provider interface or routing layer |
| `upstream` | Response cache policy, request deduplication, HTTP execution, retries, quotas and per-token concurrency |
| `auth` | JWT verification and encrypted provider token persistence |
| `database` | SQLite connection, pragmas and schema migrations |
| `backend` | PDD membership and repository lookups |
| `model`, `config` | Shared domain data and application settings |

`api` calls `pulls` for PR operations and `repos` for content at a known commit.
`pulls` uses `repos` when a tree must first be resolved from a pull's head.
Both components receive the concrete GitHub/GitLab adapters at assembly time.
The adapters call `upstream` directly for outbound requests. SQLite queries
for cached responses live in `upstream/storage.go`; token queries and encryption
live in `auth`, beside the behavior that owns them.

Interfaces are defined by consumers for the capabilities they actually use:
the HTTP collaborators, pull request providers, repository content providers
and response cache storage. There is no interface for every struct, and the
private HTTP executor uses concrete quota/retry implementations.

When adding a feature, start with its owning component. Keep its policy and
storage operations together, use private functions or files for implementation
details, and create a new package only when it has a separate responsibility.

This organization preserves existing HTTP routes, JSON bodies, configuration
keys, database schema, encryption format, TTL/ETag behavior and stale fallback.
It does not introduce a new refresh policy. Internal Go import paths changed;
the former `service`, `store`, `app` and `pkg/gitprovider` packages are removed.

See [the gateway API documentation](cmd/git-gateway/README.md) for endpoint details.

## Validation

```bash
go test ./...
go vet ./...
go test -race ./...
```
