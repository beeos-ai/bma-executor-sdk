package bmaexecutor

// Typed protocol methods (ADR 0012 §4.3): the Requests, Calls and
// Notifications tables in methods_gen.go name every contract method a client
// serves or sends with its params and result types.
//
//	bmaexecutor.Requests.ProcessList.Handle(client, func(ctx context.Context, p bmaexecutor.ProcessListParams) (bmaexecutor.ProcessListResult, error) { ... })
//	ack, err := bmaexecutor.Calls.SessionReady.Call(ctx, client, params)
//	err = bmaexecutor.Notifications.ToolProgress.Notify(ctx, client, params)

import "context"

// RequestMethod is a harness-to-executor request with params P and result R.
type RequestMethod[P, R any] struct{ Method string }

// Handle serves the request on client, replacing an earlier handler. Params
// that do not decode are answered -32602; return an *RPCError for another
// JSON-RPC error. RequestID(ctx) is the request's JSON-RPC id.
func (m RequestMethod[P, R]) Handle(client *Client, handler func(ctx context.Context, params P) (R, error)) {
	client.onRequest(m.Method, decodeThen(handler))
}

// CallMethod is an executor-to-harness request with params P and result R.
type CallMethod[P, R any] struct{ Method string }

// Call sends the request on the client's control channel and decodes its
// result; a JSON-RPC error answer is an *RPCError.
func (m CallMethod[P, R]) Call(ctx context.Context, client *Client, params P) (R, error) {
	var result R
	err := client.callInto(ctx, m.Method, params, &result)
	return result, err
}

// NotificationMethod is an executor-to-harness notification with params P.
type NotificationMethod[P any] struct{ Method string }

// Notify sends the notification on the client's control channel.
func (m NotificationMethod[P]) Notify(ctx context.Context, client *Client, params P) error {
	return client.notify(ctx, m.Method, params)
}
