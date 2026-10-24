// Package gitprovider holds the shared GitHub / GitLab request policy described in
// docs/adr/0001-git-provider-request-strategy.md.
//
// The policy is split into small packages:
//
//   - ratelimit tracks the remaining API quota per provider host and token.
//   - retry classifies failures and runs the bounded exponential backoff with jitter.
//   - cache stores response bodies with an ETag and a fresh/stale lifetime.
//   - client combines the three with request deduplication and conditional requests.
//
// This package defines the typed errors shared by all of them.
package gitprovider
