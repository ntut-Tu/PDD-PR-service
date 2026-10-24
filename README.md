# Services

Go module `pdd/services` holding PDD add-on microservices.

| Path | Content |
| --- | --- |
| `cmd/<service>/` | Entry point of one executable service |
| `pkg/` | Shared packages used by more than one service |
| `images/<service>/Dockerfile` | Container image of one service |

```bash
go test ./...
go vet ./...
```
