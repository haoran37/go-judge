package processor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/criyle/go-judge/internal/hnieoj/auth"
	"github.com/criyle/go-judge/internal/hnieoj/logging"
	"github.com/criyle/go-judge/internal/hnieoj/model"
)

// 服务端认为 token 已过期、本地时钟落后时，授权判断必须使用服务端校正时间。
func TestFocusedCredentialExpiryUsesInjectedServerClock(t *testing.T) {
	serverNow := time.Now().Add(2 * time.Hour)
	cred := &auth.Credential{AccessToken: "token", AccessExpiry: time.Now().Add(time.Hour).UnixMilli()}
	p := New(nil, nil, nil, cred, logging.NopLogger{}, nil)
	p.SetNow(func() time.Time { return serverNow })

	err := p.Process(context.Background(), model.Task{SubmissionID: "s", JudgeTaskID: "j"})
	var retryable ErrRetryable
	if !errors.As(err, &retryable) {
		t.Fatalf("expected retryable credential-expiry rejection, got %v", err)
	}
}
