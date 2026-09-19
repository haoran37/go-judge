package wss

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/criyle/go-judge/internal/hnieoj/auth"
	"github.com/criyle/go-judge/internal/hnieoj/logging"
	"github.com/criyle/go-judge/internal/hnieoj/protocol"
	"github.com/criyle/go-judge/internal/hnieoj/resultqueue"
)

// Run 维护到服务端的 WSS 会话：认证成功后 RESUME→READY，掉线指数抖动退避重连。
// 认证被明确拒绝时立即返回，绝不自动另注册绕过。
func (c *Client) Run(ctx context.Context) error {
	c.rootMu.Lock()
	c.rootCtx = ctx
	c.rootMu.Unlock()
	defer close(c.connDone)

	backoff := c.opts.ConnectMinBackoff
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		authenticated, err := c.session(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			if auth.IsDenied(err) {
				c.logger.Warn("wss authentication rejected; not re-enrolling", logging.Error(err))
				c.triggerRevoked(err.Error())
				return err
			}
			if auth.IsPermanent(err) {
				c.logger.Warn("wss session permanently rejected", logging.Error(err))
				return err
			}
		}
		if authenticated {
			backoff = c.opts.ConnectMinBackoff
		}
		c.logger.Warn("wss session ended, reconnecting", logging.Error(err),
			logging.String("backoff", backoff.String()))
		if !sleepWithJitter(ctx, backoff) {
			return ctx.Err()
		}
		backoff *= 2
		if backoff > c.opts.ConnectMaxBackoff {
			backoff = c.opts.ConnectMaxBackoff
		}
	}
}

// authCandidate 是一次初始认证允许使用的本地 key。
// 每轮最多两个：当前 key 与已持久化且服务端已返回 keyId 的 pending 轮换 key。
type authCandidate struct {
	keyID   string
	pending bool
}

// authCandidates 返回有限候选。pending 只有服务端已经返回 keyId 时才可尝试；
// 绝不在本地创建新身份/密钥/Bootstrap，也不使用任何未持久化的 key。
func (c *Client) authCandidates() []authCandidate {
	current := c.store.CurrentKeyID()
	candidates := make([]authCandidate, 0, 2)
	if current != "" {
		candidates = append(candidates, authCandidate{keyID: current})
	}
	if pending := c.store.Pending(); pending != nil && pending.NewKeyID != "" && pending.NewKeyID != current {
		candidates = append(candidates, authCandidate{keyID: pending.NewKeyID, pending: true})
	}
	if len(candidates) == 0 {
		candidates = append(candidates, authCandidate{})
	}
	return candidates
}

// session 用有限候选 key 完成一次连接认证。每个候选都使用全新的物理连接与服务端
// 主动下发的 initial AUTH_CHALLENGE；绝不在未认证 socket 上发 AUTH_REFRESH。
// 只有候选被服务端权威拒绝时才尝试下一个；网络/超时等临时错误交回 Run 退避重试。
func (c *Client) session(ctx context.Context) (bool, error) {
	candidates := c.authCandidates()
	var lastErr error
	for i, cand := range candidates {
		authenticated, err := c.sessionWithCandidate(ctx, cand)
		if err == nil || authenticated {
			return authenticated, err
		}
		// 只有权威拒绝（服务端 retryable=false / 身份被拒）才换下一个候选；
		// 网络/超时等临时错误按现有有界退避整体重试，绝不当成 key 被拒。
		if !auth.IsDenied(err) && !auth.IsPermanent(err) {
			return false, err
		}
		lastErr = err
		if i+1 >= len(candidates) {
			break
		}
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		c.logger.Warn("initial authentication rejected, trying next persisted key candidate",
			logging.String("keyId", cand.keyID), logging.Error(err))
	}
	return false, lastErr
}

// sessionWithCandidate 建立并运行一次连接，直到断开或 ctx 取消。返回是否成功完成认证。
func (c *Client) sessionWithCandidate(ctx context.Context, cand authCandidate) (bool, error) {
	conn, err := c.dial(ctx)
	if err != nil {
		return false, err
	}
	s := newSession(conn)
	c.cur.Store(s)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); c.writeLoop(s) }()
	go func() { defer wg.Done(); c.readLoop(s) }()
	defer func() {
		s.close(nil)
		_ = conn.Close()
		wg.Wait()
		if c.cur.CompareAndSwap(s, nil) {
			// 当前会话已清空。
		}
	}()

	authCtx, cancelAuth := context.WithTimeout(ctx, c.opts.AuthDeadline)
	err = c.authenticate(authCtx, s, cand)
	cancelAuth()
	if err != nil {
		return false, err
	}
	if err := c.resume(ctx, s); err != nil {
		return true, err
	}
	// 只有认证 + RESUME 完成后才发布业务可用，避免 worker 在新连接上抢跑。
	s.markReady()

	loopsCtx, cancelLoops := context.WithCancel(ctx)
	var loops sync.WaitGroup
	loops.Add(5)
	go func() { defer loops.Done(); c.heartbeatLoop(loopsCtx, s) }()
	go func() { defer loops.Done(); c.resultLoop(loopsCtx, s) }()
	go func() { defer loops.Done(); c.refreshLoop(loopsCtx, s) }()
	go func() { defer loops.Done(); c.readyLoop(loopsCtx, s) }()
	go func() { defer loops.Done(); c.pruneLoop(loopsCtx) }()
	c.notifyReady()
	c.notifyResult()

	select {
	case <-ctx.Done():
	case <-s.done:
	}
	cancelLoops()
	loops.Wait()
	if ctx.Err() != nil {
		return true, ctx.Err()
	}
	return true, s.errValue()
}

// dial 建立 WSS 连接；TLS 校验由 net/http 默认执行，绝不跳过证书/hostname 校验。
func (c *Client) dial(ctx context.Context) (*websocket.Conn, error) {
	target := normalizeWSURL(c.opts.URL)
	dialer := websocket.Dialer{
		HandshakeTimeout: 15 * time.Second,
		TLSClientConfig:  c.opts.TLSConfig,
	}
	conn, resp, err := dialer.DialContext(ctx, target, nil)
	if err != nil {
		if resp != nil {
			return nil, fmt.Errorf("wss dial failed: http %d: %w", resp.StatusCode, err)
		}
		if isTLSError(err) {
			return nil, fmt.Errorf("wss TLS verification failed: %w", err)
		}
		return nil, err
	}
	return conn, nil
}

func normalizeWSURL(raw string) string {
	raw = strings.TrimSpace(raw)
	switch {
	case strings.HasPrefix(raw, "https://"):
		return "wss://" + strings.TrimPrefix(raw, "https://")
	case strings.HasPrefix(raw, "http://"):
		return "ws://" + strings.TrimPrefix(raw, "http://")
	default:
		return raw
	}
}

func isTLSError(err error) bool {
	var recordHeaderError tls.RecordHeaderError
	if errors.As(err, &recordHeaderError) {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "tls") ||
		strings.Contains(strings.ToLower(err.Error()), "certificate")
}

func (c *Client) writeLoop(s *session) {
	for {
		select {
		case data := <-s.sendCh:
			_ = s.conn.SetWriteDeadline(time.Now().Add(c.opts.WriteTimeout))
			if err := s.conn.WriteMessage(websocket.TextMessage, data); err != nil {
				s.close(err)
				return
			}
		case <-s.done:
			return
		}
	}
}

func (c *Client) onDisconnect(s *session) {
	s.close(nil)
}

// authenticate 等待服务端在新建连接上主动下发的 initial AUTH_CHALLENGE 并完成
// AUTH_RESPONSE→AUTH_OK；cand 决定使用当前 key 还是已持久化的 pending 新 key。
func (c *Client) authenticate(ctx context.Context, s *session, cand authCandidate) error {
	var challenge *protocol.Envelope
	select {
	case challenge = <-s.initialAuth:
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
		return s.errValue()
	}
	stream := s.register(challenge.RequestID)
	defer s.unregister(challenge.RequestID)
	return c.respondWithKey(ctx, s, challenge.RequestID, stream, challenge, false, cand.keyID, cand.pending)
}

// respondToChallenge 在既有 requestId 上完成 CHALLENGE→RESPONSE→OK，
// 初次认证与 AUTH_REFRESH 都复用同一 requestId。
func (c *Client) respondToChallenge(ctx context.Context, s *session, reqID string, stream *responseStream, challengeEnv *protocol.Envelope, refresh bool) error {
	return c.respondWithKey(ctx, s, reqID, stream, challengeEnv, refresh, "", false)
}

// respondWithKey 用指定 keyId（空为当前 key；pending=true 时用轮换中 pending 私钥）签名。
func (c *Client) respondWithKey(ctx context.Context, s *session, reqID string, stream *responseStream, challengeEnv *protocol.Envelope, refresh bool, keyID string, pending bool) error {
	var challenge ChallengePayload
	if err := json.Unmarshal(challengeEnv.Payload, &challenge); err != nil {
		return fmt.Errorf("malformed AUTH_CHALLENGE: %w", err)
	}
	if challenge.ChallengeID == "" || challenge.Nonce == "" || challenge.Audience == "" {
		return errors.New("AUTH_CHALLENGE missing challengeId/nonce/audience")
	}
	// 绝不盲签任意 audience：challenge 的 audience 必须匹配本地配置。
	if c.opts.Audience != "" && challenge.Audience != c.opts.Audience {
		return fmt.Errorf("AUTH_CHALLENGE audience %q does not match configured audience %q", challenge.Audience, c.opts.Audience)
	}
	if challenge.ExpiresAt > 0 && c.ServerNow().UTC().UnixMilli() >= challenge.ExpiresAt {
		return errors.New("AUTH_CHALLENGE expired")
	}
	nodeID := c.store.NodeID()
	if nodeID == "" {
		return errors.New("node is not enrolled")
	}
	if keyID == "" {
		keyID = c.store.CurrentKeyID()
	}
	if keyID == "" {
		return errors.New("node is not enrolled")
	}
	fields := protocol.AuthFields(challenge.Audience, challenge.ChallengeID, challenge.Nonce, nodeID, keyID)
	var (
		signature string
		err       error
	)
	if pending {
		signature, err = c.store.SignPending(fields...)
	} else {
		signature, err = c.store.SignWith(keyID, fields...)
	}
	if err != nil {
		return fmt.Errorf("sign AUTH_RESPONSE: %w", err)
	}
	payload := AuthResponsePayload{NodeID: nodeID, KeyID: keyID, ChallengeID: challenge.ChallengeID, Signature: signature}
	if err := c.send(s, protocol.TypeAuthResponse, reqID, payload, c.opts.ControlFrameBytes); err != nil {
		return err
	}
	select {
	case env, ok := <-stream.ch:
		if !ok {
			return s.errValue()
		}
		if env.Type == protocol.TypeError {
			return parseServerError(env)
		}
		if env.Type != protocol.TypeAuthOK {
			return fmt.Errorf("expected AUTH_OK, got %s", env.Type)
		}
		var authOK AuthOKPayload
		if err := json.Unmarshal(env.Payload, &authOK); err != nil {
			return fmt.Errorf("malformed AUTH_OK: %w", err)
		}
		return c.applyAuthOK(&authOK, refresh)
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
		return s.errValue()
	}
}

// refresh 在同一连接上续授权；不改变 sessionEpoch，不中断在途任务。
func (c *Client) refresh(ctx context.Context, s *session) error {
	reqID := randomRequestID()
	stream := s.register(reqID)
	defer s.unregister(reqID)
	if err := c.send(s, protocol.TypeAuthRefresh, reqID, map[string]string{}, c.opts.ControlFrameBytes); err != nil {
		return err
	}
	var challengeEnv *protocol.Envelope
	select {
	case env, ok := <-stream.ch:
		if !ok {
			return s.errValue()
		}
		if env.Type == protocol.TypeError {
			return parseServerError(env)
		}
		challengeEnv = env
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
		return s.errValue()
	}
	return c.respondToChallenge(ctx, s, reqID, stream, challengeEnv, true)
}

// applyAuthOK 更新短期授权与 credential Snapshot，并校验 server 授权与本地身份一致。
func (c *Client) applyAuthOK(payload *AuthOKPayload, refresh bool) error {
	if payload.NodeID == "" || payload.KeyID == "" || payload.AccessToken == "" {
		return errors.New("AUTH_OK missing nodeId/keyId/accessToken")
	}
	if localNode := c.store.NodeID(); localNode != "" && payload.NodeID != localNode {
		return fmt.Errorf("AUTH_OK nodeId %q does not match local nodeId %q", payload.NodeID, localNode)
	}
	if !c.store.HasSigningKey(payload.KeyID) {
		return fmt.Errorf("AUTH_OK keyId %q has no local private key", payload.KeyID)
	}
	expiry := payload.AccessExpiresAt
	if expiry <= 0 {
		expiry = auth.JWTExpiryMillis(payload.AccessToken)
	}
	if expiry <= 0 {
		return errors.New("AUTH_OK missing accessExpiresAt")
	}
	epoch := payload.SessionEpoch
	if epoch <= 0 {
		return errors.New("AUTH_OK missing positive sessionEpoch")
	}
	if refresh {
		// 续授权不改变 sessionEpoch；若服务端返回了不同的 epoch，说明会话已被接管。
		if current := c.epoch.Load(); current != 0 && epoch != current {
			return fmt.Errorf("AUTH_REFRESH changed sessionEpoch %d -> %d", current, epoch)
		}
		epoch = c.epoch.Load()
	} else {
		c.epoch.Store(epoch)
	}
	cred := &auth.Credential{
		NodeID:                  payload.NodeID,
		KeyID:                   payload.KeyID,
		NodeType:                c.opts.NodeType,
		Audience:                c.opts.Audience,
		AccessToken:             payload.AccessToken,
		AccessExpiry:            expiry,
		SessionEpoch:            epoch,
		AuthorizationUntil:      payload.AuthorizationUntil,
		MaxConcurrency:          payload.MaxConcurrency,
		SupportedJudgeModes:     payload.SupportedJudgeModes,
		HeartbeatIntervalMillis: payload.HeartbeatIntervalMillis,
	}
	c.cred.Replace(cred)
	return nil
}

// resume 在 READY 之前恢复在途 attempt：合法 resume 保持 attempt，拒绝/完成/省略立即取消。
// 恢复清单同时包含 registry 在途 attempt 与磁盘未 ACK 的终态结果，并按 key 去重。
func (c *Client) resume(ctx context.Context, s *session) error {
	inFlight := map[string]struct{}{}
	attempts := make([]ResumeAttempt, 0, c.registry.Len())
	appendAttempt := func(submissionID, judgeTaskID, attemptID string) {
		if submissionID == "" || judgeTaskID == "" || attemptID == "" {
			return
		}
		key := taskKey(submissionID, judgeTaskID, attemptID)
		if _, ok := inFlight[key]; ok {
			return
		}
		inFlight[key] = struct{}{}
		attempts = append(attempts, ResumeAttempt{SubmissionID: submissionID, JudgeTaskID: judgeTaskID, AttemptID: attemptID})
	}
	for _, attempt := range c.registry.Snapshot() {
		appendAttempt(attempt.SubmissionID, attempt.JudgeTaskID, attempt.AttemptID)
	}
	if c.queue != nil {
		for _, rec := range c.queue.Peek() {
			appendAttempt(rec.SubmissionID, rec.JudgeTaskID, rec.AttemptID)
		}
	}

	env, err := c.request(ctx, s, protocol.TypeResumeTasks, ResumeTasksPayload{Attempts: attempts}, c.opts.ControlFrameBytes)
	if err != nil {
		return fmt.Errorf("RESUME_TASKS: %w", err)
	}
	if env.Type != protocol.TypeResumeResult {
		return fmt.Errorf("expected RESUME_RESULT, got %s", env.Type)
	}
	var result ResumeResultPayload
	if err := json.Unmarshal(env.Payload, &result); err != nil {
		return fmt.Errorf("malformed RESUME_RESULT: %w", err)
	}
	epoch := c.epoch.Load()
	now := c.ServerNow().UnixMilli()
	resumed := map[string]ResumedTask{}
	for _, task := range result.Resumed {
		if task.SubmissionID == "" || task.JudgeTaskID == "" || task.AttemptID == "" {
			continue
		}
		resumed[taskKey(task.SubmissionID, task.JudgeTaskID, task.AttemptID)] = task
	}
	rejected := map[string]bool{}
	for _, item := range result.Rejected {
		key := taskKey(item.SubmissionID, item.JudgeTaskID, item.AttemptID)
		rejected[key] = true
		c.logger.Warn("resume rejected; cancelling local attempt",
			logging.String("submissionId", item.SubmissionID), logging.String("reason", item.Reason))
		c.registry.Cancel(item.SubmissionID, item.JudgeTaskID, item.AttemptID)
	}
	for key, task := range resumed {
		if _, active := inFlight[key]; !active {
			// 只有磁盘结果、没有本地执行：无需续租，由 resultLoop 重传。
			continue
		}
		if rejected[key] {
			// 同时出现在 resumed 与 rejected 时以拒绝为准：绝不复活已取消的执行。
			c.registry.Cancel(task.SubmissionID, task.JudgeTaskID, task.AttemptID)
			continue
		}
		if task.Completed {
			// 服务端已完成该任务，本地在途执行必须取消，结果由持久队列重传。
			c.registry.Cancel(task.SubmissionID, task.JudgeTaskID, task.AttemptID)
			continue
		}
		if task.LeaseUntil <= 0 || task.LeaseUntil <= now {
			// 后端是租约唯一权威：缺失/非法/过期租约不得在本地合成延长，
			// 否则无效 resume 会凭空获得额外执行时间，立即取消该 attempt。
			c.logger.Warn("resume returned invalid lease; cancelling local attempt",
				logging.String("submissionId", task.SubmissionID), logging.Int64("leaseUntil", task.LeaseUntil))
			c.registry.Cancel(task.SubmissionID, task.JudgeTaskID, task.AttemptID)
			continue
		}
		c.registry.Resume(task.SubmissionID, task.JudgeTaskID, task.AttemptID, task.LeaseUntil, epoch)
	}
	// 省略（既未 resumed 也未 rejected）一律取消，但不允许迟到回复复活。
	for key := range inFlight {
		if _, ok := resumed[key]; ok {
			continue
		}
		if rejected[key] {
			continue
		}
		parts := strings.SplitN(key, "\x00", 3)
		if len(parts) != 3 {
			continue
		}
		c.registry.Cancel(parts[0], parts[1], parts[2])
	}
	c.logger.Info("resume completed",
		logging.Int("resumed", len(resumed)), logging.Int("rejected", len(result.Rejected)))
	return nil
}

func taskKey(submissionID, judgeTaskID, attemptID string) string {
	return submissionID + "\x00" + judgeTaskID + "\x00" + attemptID
}

// sendReady 上报可用槽位；服务端批准额度与本地配置双重约束，排空或队列满时为 0。
func (c *Client) sendReady(s *session) {
	if c.Draining() {
		return
	}
	slots := c.availableSlots()
	c.ready.Store(int32(slots))
	reqID := randomRequestID()
	if err := c.send(s, protocol.TypeReady, reqID, ReadyPayload{AvailableSlots: slots}, c.opts.ControlFrameBytes); err != nil {
		c.logger.Warn("failed to send READY", logging.Error(err))
	}
}

func (c *Client) availableSlots() int {
	slots := c.approvedSlots()
	slots -= c.registry.Len()
	if slots < 0 {
		slots = 0
	}
	if c.queue != nil && c.queue.Full() {
		slots = 0
	}
	return slots
}

// ReadySlots 返回最近上报的可用槽位。
func (c *Client) ReadySlots() int { return int(c.ready.Load()) }

// BeginDrain 发送 NODE_DRAIN 并停止 READY；在途任务的续租/结果通道保持存活。
func (c *Client) BeginDrain(ctx context.Context) error {
	c.SetDraining()
	c.notifyReady()
	s := c.activeSession()
	if s == nil {
		return ErrDisconnected
	}
	reqID := randomRequestID()
	return c.send(s, protocol.TypeNodeDrain, reqID, NodeStatePayload{Status: "DRAINING", Draining: true}, c.opts.ControlFrameBytes)
}

func (c *Client) readyLoop(ctx context.Context, s *session) {
	debounce := time.NewTicker(200 * time.Millisecond)
	defer debounce.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.readyNotify:
		case <-debounce.C:
			continue
		}
		// 合并短时间内的多次通知。
		select {
		case <-ctx.Done():
			return
		case <-time.After(50 * time.Millisecond):
		}
		c.sendReady(s)
	}
}

func (c *Client) heartbeatLoop(ctx context.Context, s *session) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.done:
			return
		case <-timer.C:
		}
		c.sendHeartbeat(ctx, s)
		timer.Reset(c.heartbeatInterval())
	}
}

func (c *Client) heartbeatInterval() time.Duration {
	if millis := c.cred.Snapshot().HeartbeatIntervalMillis; millis > 0 {
		return time.Duration(millis) * time.Millisecond
	}
	return c.opts.HeartbeatFallback
}

func (c *Client) sendHeartbeat(ctx context.Context, s *session) {
	draining := c.Draining()
	var payload json.RawMessage
	if c.opts.HeartbeatPayload != nil {
		built, err := c.opts.HeartbeatPayload(draining)
		if err != nil {
			c.logger.Warn("build heartbeat payload failed", logging.Error(err))
			return
		}
		payload = built
	} else {
		payload, _ = json.Marshal(map[string]any{"draining": draining})
	}
	reqID := randomRequestID()
	stream := s.register(reqID)
	defer s.unregister(reqID)
	if err := c.sendRaw(s, mustEnvelope(protocol.TypeHeartbeat, reqID, payload, c.ServerNow()), c.opts.ControlFrameBytes); err != nil {
		return
	}
	select {
	case env, ok := <-stream.ch:
		if !ok {
			return
		}
		if env.Type == protocol.TypeError {
			if err := parseServerError(env); auth.IsDenied(err) {
				c.logger.Warn("heartbeat rejected by server", logging.Error(err))
				c.triggerRevoked(err.Error())
				s.close(err)
			}
		}
	case <-ctx.Done():
	case <-s.done:
	case <-time.After(c.opts.RequestTimeout):
		c.logger.Warn("heartbeat ACK timed out")
	}
}

func (c *Client) resultLoop(ctx context.Context, s *session) {
	ticker := time.NewTicker(c.opts.ResultRetryBackoff)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.resultNotify:
		case <-ticker.C:
		}
		c.deliverResults(ctx, s)
	}
}

// deliverResults 逐条投递终态结果，只有收到与记录完全匹配的 TASK_RESULT_ACK 才删除。
// 单条失败不会阻塞后续结果；永久拒绝保留在队列中由 TTL/审计兜底。
func (c *Client) deliverResults(ctx context.Context, s *session) {
	if c.queue == nil {
		return
	}
	for _, rec := range c.queue.Peek() {
		if ctx.Err() != nil {
			return
		}
		c.queue.MarkAttempt(rec.ID)
		env, err := c.request(ctx, s, protocol.TypeTaskResult, TaskEventPayload{
			SubmissionID: rec.SubmissionID,
			Event:        rec.Event,
		}, c.opts.TaskFrameBytes)
		if err != nil {
			c.logger.Warn("result delivery failed, will retry",
				logging.String("submissionId", rec.SubmissionID), logging.Error(err))
			continue
		}
		if env.Type != protocol.TypeTaskResultAck {
			c.logger.Warn("unexpected response for TASK_RESULT, keeping durable result",
				logging.String("submissionId", rec.SubmissionID), logging.String("type", env.Type))
			continue
		}
		var ack TaskResultAckPayload
		if err := json.Unmarshal(env.Payload, &ack); err != nil {
			c.logger.Warn("malformed TASK_RESULT_ACK", logging.String("submissionId", rec.SubmissionID))
			continue
		}
		if ack.SubmissionID != rec.SubmissionID || ack.JudgeTaskID != rec.JudgeTaskID || ack.AttemptID != rec.AttemptID {
			c.logger.Warn("TASK_RESULT_ACK identity mismatch, keeping durable result",
				logging.String("submissionId", rec.SubmissionID))
			continue
		}
		if err := c.queue.Ack(rec.ID); err != nil {
			c.logger.Warn("result queue ack failed", logging.Error(err))
			continue
		}
		c.logger.Info("result acknowledged", logging.String("submissionId", rec.SubmissionID))
		c.notifyReady()
	}
}

// pruneLoop 周期性清理超过 TTL 的终态结果并审计。
func (c *Client) pruneLoop(ctx context.Context) {
	if c.queue == nil || c.opts.ResultTTL <= 0 {
		return
	}
	interval := c.opts.ResultTTL / 4
	if interval < time.Minute {
		interval = time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			removed, err := c.queue.Prune(c.opts.Now())
			if err != nil {
				c.logger.Warn("result queue prune failed", logging.Error(err))
			}
			if removed > 0 {
				c.logger.Warn("expired unacknowledged results discarded by TTL",
					logging.Int("count", removed))
			}
		}
	}
}

func (c *Client) refreshLoop(ctx context.Context, s *session) {
	for {
		cred := c.cred.Snapshot()
		wait := refreshDelay(cred.AccessExpiry, c.ServerNow())
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-s.done:
			timer.Stop()
			return
		case <-c.refreshNotify:
			timer.Stop()
		case <-timer.C:
		}
		refreshCtx, cancel := context.WithTimeout(ctx, c.opts.RequestTimeout)
		err := c.refresh(refreshCtx, s)
		cancel()
		if err != nil {
			if auth.IsDenied(err) || auth.IsPermanent(err) {
				c.logger.Warn("token refresh rejected", logging.Error(err))
				if auth.IsDenied(err) {
					c.triggerRevoked(err.Error())
				}
				s.close(err)
				return
			}
			c.logger.Warn("token refresh failed", logging.Error(err))
			if c.cred.Expired(c.ServerNow()) {
				// 过期且无法续授权：关闭连接，重连时用有效密钥重新认证（不重新注册）。
				c.logger.Warn("access token expired without refresh, forcing reconnect")
				s.close(err)
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(c.opts.ResultRetryBackoff):
			}
			continue
		}
		c.logger.Info("access token refreshed", logging.String("keyId", c.cred.Snapshot().KeyID))
	}
}

// refreshDelay 在到期前 remaining/3 处触发续授权；now 为服务端校正后的时间。
func refreshDelay(expiryMillis int64, now time.Time) time.Duration {
	if expiryMillis <= 0 {
		return 5 * time.Minute
	}
	remaining := time.UnixMilli(expiryMillis).Sub(now)
	if remaining <= 0 {
		return time.Second
	}
	delay := remaining * 2 / 3
	if delay < time.Second {
		return time.Second
	}
	return delay
}

// ResultEnqueuer 允许 processor 通过 reporter 接口写入终态结果。
func (c *Client) queueRecord(submissionID, judgeTaskID, attemptID string, event json.RawMessage) error {
	if c.queue == nil {
		return errors.New("result queue is not configured")
	}
	if err := c.queue.Enqueue(resultqueue.Record{
		ID:           taskKey(submissionID, judgeTaskID, attemptID),
		SubmissionID: submissionID,
		JudgeTaskID:  judgeTaskID,
		AttemptID:    attemptID,
		Event:        event,
		EnqueuedAt:   c.opts.Now().UTC().UnixMilli(),
	}); err != nil {
		// 队列满/磁盘失败：立即用 0 槽位 READY 停止新分配并显式失败，不丢结果。
		c.logger.Warn("persisting terminal result failed; stopping new task allocation",
			logging.String("submissionId", submissionID), logging.Error(err))
		c.notifyReady()
		return err
	}
	c.notifyResult()
	return nil
}

func mustEnvelope(msgType, reqID string, payload json.RawMessage, now time.Time) []byte {
	env := protocol.Envelope{Version: protocol.Version, Type: msgType, RequestID: reqID, Timestamp: now.UTC().UnixMilli(), Payload: payload}
	data, _ := json.Marshal(env)
	return data
}

func sleepWithJitter(ctx context.Context, base time.Duration) bool {
	if base <= 0 {
		return ctx.Err() == nil
	}
	// 在 [base/2, base] 抖动，避免所有节点同时重连。
	half := base / 2
	jitter := time.Duration(0)
	if half > 0 {
		jitter = time.Duration(time.Now().UnixNano() % int64(half+1))
	}
	timer := time.NewTimer(base - half + jitter)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
