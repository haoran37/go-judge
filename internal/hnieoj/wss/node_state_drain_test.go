package wss

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/criyle/go-judge/internal/hnieoj/auth"
	"github.com/criyle/go-judge/internal/hnieoj/protocol"
)

func nodeStateEnvelope(t *testing.T, status string, draining bool) *protocol.Envelope {
	t.Helper()
	raw, err := json.Marshal(NodeStatePayload{Status: status, Draining: draining})
	if err != nil {
		t.Fatal(err)
	}
	return &protocol.Envelope{Type: protocol.TypeNodeState, RequestID: "state", Payload: raw}
}

func newStateClient(onRevoked func(string)) *Client {
	return New(nil, &auth.Credential{}, nil, nil, nopLogger{}, Options{OnRevoked: onRevoked})
}

// TestNodeStateRemoteDrainReversible 覆盖 AC1：远端 DRAINING 置位，权威 ACTIVE 清除。
func TestNodeStateRemoteDrainReversible(t *testing.T) {
	c := newStateClient(nil)
	c.handleNodeState(nodeStateEnvelope(t, "DRAINING", true))
	if !c.Draining() {
		t.Fatal("remote DRAINING did not stop allocation")
	}
	c.handleNodeState(nodeStateEnvelope(t, "ACTIVE", false))
	if c.Draining() {
		t.Fatal("authoritative ACTIVE did not clear remote drain")
	}
}

// TestNodeStateLocalStopSurvivesRemoteActive 覆盖 AC2：本地停止不可逆，
// 远端 ACTIVE 只清除远端标志，绝不撤销本地停止。
func TestNodeStateLocalStopSurvivesRemoteActive(t *testing.T) {
	c := newStateClient(nil)
	c.SetDraining()
	c.handleNodeState(nodeStateEnvelope(t, "DRAINING", true))
	c.handleNodeState(nodeStateEnvelope(t, "ACTIVE", false))
	c.handleNodeState(nodeStateEnvelope(t, "ACTIVE", false))
	if !c.Draining() {
		t.Fatal("remote ACTIVE undid the local stop")
	}
}

// TestNodeStateUnknownStatusDoesNotResume 覆盖 AC3：未知/非法/畸形状态不得恢复。
func TestNodeStateUnknownStatusDoesNotResume(t *testing.T) {
	c := newStateClient(nil)
	c.handleNodeState(nodeStateEnvelope(t, "DRAINING", true))
	for _, status := range []string{"MAINTENANCE", "PAUSED", "", "0"} {
		c.handleNodeState(nodeStateEnvelope(t, status, false))
	}
	if !c.Draining() {
		t.Fatal("unknown NODE_STATE status cleared remote drain")
	}
	c.handleNodeState(&protocol.Envelope{Type: protocol.TypeNodeState, Payload: []byte("{")})
	if !c.Draining() {
		t.Fatal("malformed NODE_STATE cleared remote drain")
	}
}

// TestNodeStateActiveWithDrainingFlagDoesNotResume 覆盖 AC3：
// ACTIVE 却带 draining=true 属不一致信号，按更保守的排空处理，绝不恢复。
func TestNodeStateActiveWithDrainingFlagDoesNotResume(t *testing.T) {
	c := newStateClient(nil)
	c.handleNodeState(nodeStateEnvelope(t, "DRAINING", true))
	c.handleNodeState(nodeStateEnvelope(t, "ACTIVE", true))
	if !c.Draining() {
		t.Fatal("ACTIVE with draining=true resumed allocation")
	}
}

// TestNodeStateRepeatedMessagesIdempotent 覆盖 AC3：同状态重复消息幂等。
func TestNodeStateRepeatedMessagesIdempotent(t *testing.T) {
	c := newStateClient(nil)
	c.handleNodeState(nodeStateEnvelope(t, "DRAINING", true))
	c.handleNodeState(nodeStateEnvelope(t, "DRAINING", true))
	if !c.Draining() {
		t.Fatal("repeated DRAINING lost the drain state")
	}
	c.handleNodeState(nodeStateEnvelope(t, "ACTIVE", false))
	c.handleNodeState(nodeStateEnvelope(t, "ACTIVE", false))
	if c.Draining() {
		t.Fatal("repeated ACTIVE did not stay resumed")
	}
}

// TestNodeStateInactiveRevokesImmediately 覆盖 AC3：
// DISABLED/REVOKED/EXPIRED 仍立即触发吊销。
func TestNodeStateInactiveRevokesImmediately(t *testing.T) {
	for _, status := range []string{"DISABLED", "REVOKED", "EXPIRED"} {
		t.Run(status, func(t *testing.T) {
			var reason string
			c := newStateClient(func(r string) { reason = r })
			c.handleNodeState(nodeStateEnvelope(t, status, false))
			if !strings.Contains(reason, status) {
				t.Fatalf("revoke reason = %q, want status %s", reason, status)
			}
		})
	}
}

// TestInboundNodeDrainIsReversibleRemoteDrain 覆盖服务端主动 NODE_DRAIN：
// 属远端可恢复排空，可由权威 ACTIVE 清除。
func TestInboundNodeDrainIsReversibleRemoteDrain(t *testing.T) {
	c := newStateClient(nil)
	c.dispatch(nil, &protocol.Envelope{Type: protocol.TypeNodeDrain, RequestID: "drain"})
	if !c.Draining() {
		t.Fatal("inbound NODE_DRAIN did not stop allocation")
	}
	c.handleNodeState(nodeStateEnvelope(t, "ACTIVE", false))
	if c.Draining() {
		t.Fatal("inbound NODE_DRAIN was not reversible by authoritative ACTIVE")
	}
}

// TestHeartbeatUsesEffectiveDraining 覆盖 AC2：
// HEARTBEAT 与 READY/ASSIGN 使用同一个有效排空状态 local||remote。
func TestHeartbeatUsesEffectiveDraining(t *testing.T) {
	var got []bool
	c := New(nil, &auth.Credential{}, nil, nil, nopLogger{}, Options{
		HeartbeatPayload: func(draining bool) (json.RawMessage, error) {
			got = append(got, draining)
			return json.RawMessage(`{}`), nil
		},
	})
	s := newSession(nil)
	s.close(nil)
	c.sendHeartbeat(context.Background(), s)
	c.handleNodeState(nodeStateEnvelope(t, "DRAINING", true))
	c.sendHeartbeat(context.Background(), s)
	c.SetDraining()
	c.handleNodeState(nodeStateEnvelope(t, "ACTIVE", false))
	c.sendHeartbeat(context.Background(), s)
	want := []bool{false, true, true}
	if len(got) != len(want) {
		t.Fatalf("heartbeat draining flags = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("heartbeat draining flags = %v, want %v", got, want)
		}
	}
}

// TestRemoteDrainDoesNotCancelInFlight 覆盖 AC1：远端排空不取消现有有效任务，
// 排空期间拒绝新分配，权威 ACTIVE 后恢复接收。
func TestRemoteDrainDoesNotCancelInFlight(t *testing.T) {
	c, s := newUnitClient(t)
	c.opts.LocalConcurrency = 2
	s.markReady()
	c.handleAssign(s, assignEnvelope(t, "s", "j", "a"))
	<-s.sendCh // TASK_ACK
	c.handleNodeState(nodeStateEnvelope(t, "DRAINING", true))
	if _, ok := c.registry.Get("s", "j", "a"); !ok {
		t.Fatal("remote drain removed the in-flight attempt")
	}
	// 排空期间新分配必须被拒绝：不注册也不 ACK。
	c.handleAssign(s, assignEnvelope(t, "s2", "j2", "a2"))
	if c.registry.Len() != 1 {
		t.Fatalf("assignment admitted while draining: registry len = %d", c.registry.Len())
	}
	select {
	case <-s.sendCh:
		t.Fatal("TASK_ASSIGN was acknowledged while draining")
	default:
	}
	// 权威 ACTIVE 后恢复接收新分配。
	c.handleNodeState(nodeStateEnvelope(t, "ACTIVE", false))
	c.handleAssign(s, assignEnvelope(t, "s2", "j2", "a2"))
	<-s.sendCh // TASK_ACK
	if c.registry.Len() != 2 {
		t.Fatalf("assignment after ACTIVE not accepted: registry len = %d", c.registry.Len())
	}
}
