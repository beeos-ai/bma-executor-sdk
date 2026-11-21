// Package conformance is the tool route conformance suite of the SDK
// (ADR-0029 §5, cases T1–T11), for the tests of anything that is a
// bmaexecutor.ToolRoute: the SDK's LocalToolRoute and PlatformToolRoute, and
// the route of a brain or hands of your own.
//
// A brain must not be able to tell which route its calls took: the same
// refusals with the same error codes, the same three outcomes (executed,
// not_forwarded, unknown), the same texts, at most one run per call id, the
// same cancel and epoch behavior. RunToolRouteConformance runs one set of
// cases against the route a Factory builds:
//
//	func TestMyRoute(t *testing.T) {
//		conformance.RunToolRouteConformance(t, func(t *testing.T, toolPolicy map[string]string) *conformance.Rig {
//			// build your route and the levers the cases need
//		})
//	}
//
// PlatformFactory builds the SDK's PlatformToolRoute against the contract
// double of the platform's tool gateway (package gatewaystub), with hands of
// your choosing (CoreHands runs the calls in a Core; the example hand does
// that). Run it to check that your hands are what the gateway expects.
//
// The package depends on the SDK's public interface and on package testing
// only. It is for tests: production code must not import it (the SDK checks
// its own, see arch_test.go). Its cases start POSIX commands, so it is not built
// on Windows.
package conformance
