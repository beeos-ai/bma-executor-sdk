// Package gatewaystub is a contract double of the platform's execute-only tool
// gateway (ADR-0005) and of the hands behind it, for tests of brains, of hands
// and of the SDK's tool routes. It serves the MCP Streamable HTTP endpoint a
// turn's tools_mcp_url points at and does what the gateway does: checks the
// bearer token against the turn's epoch, re-evaluates the tool policy, verifies
// the user's approval (one approval, one call: same tool, same input), runs
// each idempotency key at most once and answers with one of the three outcomes
// (executed, not_forwarded, unknown).
//
// It is not the platform and it is not a library for production code: it
// imports package testing, and nothing but tests may import it (the SDK checks
// that none of its own non-test code does; see arch_test.go). It depends on
// the standard library only, so it never shares an implementation with the
// routes it is used to test.
package gatewaystub
