//go:build !windows

package bmaexecutor

import (
	"context"
	"strconv"
	"testing"
	"time"
)

// turn.start is answered as soon as the turn is accepted (contract): opening
// the agent's session runs afterwards, and what goes wrong there is reported
// with the contract's turn failure (session.error with an AgentErrorCode,
// session.status_idle failed), not as a failed or late turn.start.

func slowOpen(ms int) ACPAgentOptions {
	return ACPAgentOptions{Env: []string{"BMA_ACP_FAKE_NEW_DELAY_MS=" + strconv.Itoa(ms)}}
}

func TestACPAgentAnswersTurnStartBeforeTheSessionIsOpen(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	f := startACPFixture(t, ctx, slowOpen(1500))
	began := time.Now()
	result := f.start(t, ctx, "sesn_slow", "turn_1", "hello")
	if took := time.Since(began); took > 700*time.Millisecond {
		t.Fatalf("turn.start took %v while session/new takes 1.5 s", took)
	}
	if !result.Accepted || result.TurnID != "turn_1" || result.ExecutorSessionRef != "sesn_slow" || result.Effective.Harness != "dsh" {
		t.Fatalf("result = %+v", result)
	}
	assertIdle(t, f.platform.waitIdle(t, "turn_1"), TurnOutcomeCompleted)
	if errs := sessionErrors(f.platform, "turn_1"); len(errs) != 0 {
		t.Fatalf("session errors = %+v", errs)
	}
}

// A cancel that arrives while the session is opening ends the turn at once;
// the open runs on and the next turn uses the session it left.
func TestACPAgentCancelDuringOpenEndsTheTurnAtOnce(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	f := startACPFixture(t, ctx, slowOpen(2500))
	f.start(t, ctx, "sesn_c", "turn_1", "hello")
	began := time.Now()
	canceled, err := f.executor.CancelTurn(ctx, TurnCancelParams{SessionID: "sesn_c", TurnID: "turn_1", Reason: "user_interrupt"})
	if err != nil || !canceled.Canceled {
		t.Fatalf("turn.cancel = %+v %v", canceled, err)
	}
	assertIdle(t, f.platform.waitIdle(t, "turn_1"), TurnOutcomeCanceled)
	if took := time.Since(began); took > time.Second {
		t.Fatalf("the canceled turn ended after %v, the open takes 2.5 s", took)
	}
	f.start(t, ctx, "sesn_c", "turn_2", "hello")
	assertIdle(t, f.platform.waitIdle(t, "turn_2"), TurnOutcomeCompleted)
}

// The cause of a failed open is the contract's code, in session.error.
func TestACPAgentFailedOpenIsATypedTurnFailure(t *testing.T) {
	for _, tc := range []struct {
		acpCode int
		want    string
	}{
		{-32000, AgentErrorCodeAgentAuthRequired},
		{-32602, AgentErrorCodeAgentProtocolError},
		{-32603, AgentErrorCodeAgentError},
	} {
		t.Run(tc.want, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			f := startACPFixture(t, ctx, ACPAgentOptions{Env: []string{"BMA_ACP_FAKE_NEW_ERROR=" + strconv.Itoa(tc.acpCode)}})
			f.start(t, ctx, "sesn_f", "turn_1", "hello") // accepted
			assertIdle(t, f.platform.waitIdle(t, "turn_1"), TurnOutcomeFailed)
			errs := sessionErrors(f.platform, "turn_1")
			if len(errs) != 1 || errs[0].Type != tc.want {
				t.Fatalf("session errors = %+v, want type %s", errs, tc.want)
			}
		})
	}
}

// A refusal that is certain and cheap stays a turn.start error.
func TestACPAgentStillRefusesBadParamsInTurnStart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	f := startACPFixture(t, ctx, slowOpen(1500))
	began := time.Now()
	_, err := f.executor.StartTurn(ctx, TurnStartParams{SessionID: "sesn_m", TurnID: "turn_1", InputID: "in_1", Config: &AgentTurnConfig{PermissionMode: "nonsense"},
		Input: []AgentInputBlock{{Type: AgentInputBlockTypeText, Text: "hello"}}})
	var rpc *RPCError
	if !asRPC(err, &rpc) || rpc.Code != CodeInvalidParams || time.Since(began) > 700*time.Millisecond {
		t.Fatalf("err = %v", err)
	}
}
