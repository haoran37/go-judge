package wss

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/criyle/go-judge/internal/hnieoj/auth"
	"github.com/criyle/go-judge/internal/hnieoj/identity"
	"github.com/criyle/go-judge/internal/hnieoj/logging"
	"github.com/criyle/go-judge/internal/hnieoj/model"
	"github.com/criyle/go-judge/internal/hnieoj/protocol"
	"github.com/criyle/go-judge/internal/hnieoj/resultqueue"
	"github.com/criyle/go-judge/internal/hnieoj/tasks"
)

var (
	// ErrDisconnected 表示当前 WSS 连接已断开。
	ErrDisconnected   = errors.New("wss connection closed")
	errFrameTooLarge  = errors.New("wss frame exceeds configured limit")
	errWriteQueueFull = errors.New("wss outbound queue is full")
	errAssignOverflow = errors.New("server assigned more tasks than approved concurrency")
)

// HeartbeatProvider 构造 HEARTBEAT payload（existing metrics + draining）。
type HeartbeatProvider func(draining bool) (json.RawMessage, error)

// Options 是 WSS 任务通道配置，所有边界都有默认值与硬顶。
type Options struct {
	URL                 string
	Audience            string
	NodeName            string
	NodeType            string
	LocalConcurrency    int
	SupportedJudgeModes []string

	ControlFrameBytes  int
	TaskFrameBytes     int
	AuthDeadline       time.Duration
	RequestTimeout     time.Duration
	ConnectMinBackoff  time.Duration
	ConnectMaxBackoff  time.Duration
	HeartbeatFallback  time.Duration
	ResultRetryBackoff time.Duration
	ResultTTL          time.Duration
	WriteTimeout       time.Duration

	TLSConfig *tls.Config

	HeartbeatPayload HeartbeatProvider
	OnRevoked        func(reason string)
	Now              func() time.Time
}

func (o *Options) applyDefaults() {
	if o.ControlFrameBytes <= 0 {
		o.ControlFrameBytes = protocol.DefaultControlFrameBytes
	}
	if o.TaskFrameBytes <= 0 {
		o.TaskFrameBytes = protocol.DefaultTaskFrameBytes
	}
	if o.AuthDeadline <= 0 {
		o.AuthDeadline = protocol.AuthDeadline
	}
	if o.RequestTimeout <= 0 {
		o.RequestTimeout = 30 * time.Second
	}
	if o.ConnectMinBackoff <= 0 {
		o.ConnectMinBackoff = 500 * time.Millisecond
	}
	if o.ConnectMaxBackoff < o.ConnectMinBackoff {
		o.ConnectMaxBackoff = 30 * time.Second
	}
	if o.HeartbeatFallback <= 0 {
		o.HeartbeatFallback = 30 * time.Second
	}
	if o.ResultRetryBackoff <= 0 {
		o.ResultRetryBackoff = 2 * time.Second
	}
	if o.ResultTTL <= 0 {
		o.ResultTTL = 72 * time.Hour
	}
	if o.WriteTimeout <= 0 {
		o.WriteTimeout = 10 * time.Second
	}
	if o.LocalConcurrency <= 0 {
		o.LocalConcurrency = 1
	}
	if o.Now == nil {
		o.Now = time.Now
	}
}

// Client 是单节点 WSS 任务通道客户端。
type Client struct {
	store    *identity.Store
	cred     *auth.Credential
	registry *tasks.Registry
	queue    *resultqueue.Queue
	logger   logging.Logger
	opts     Options

	assignCh      chan model.Assignment
	connDone      chan struct{}
	readyNotify   chan struct{}
	refreshNotify chan struct{}
	resultNotify  chan struct{}

	// localDraining 记录本地停止（SIGTERM / WebUI 停止 / BeginDrain），一旦置位不可逆。
	localDraining atomic.Bool
	// remoteDraining 记录服务端权威下发的排空状态，可被后续权威 ACTIVE 清除。
	remoteDraining atomic.Bool
	epoch          atomic.Int64
	offset         atomic.Int64
	ready          atomic.Int32

	cur atomic.Pointer[session]

	rootMu  sync.Mutex
	rootCtx context.Context

	revokeOnce sync.Once
}

// New 创建 WSS 客户端。
func New(store *identity.Store, cred *auth.Credential, registry *tasks.Registry, queue *resultqueue.Queue, logger logging.Logger, opts Options) *Client {
	opts.applyDefaults()
	if registry != nil && queue != nil {
		// attempt 离开在途表（完成/取消）时释放其预留的结果容量；
		// 已转换为终态记录的预留由 Enqueue 先消费，此回调是 no-op。
		registry.SetDoneObserver(func(submissionID, judgeTaskID, attemptID string) {
			queue.Release(taskKey(submissionID, judgeTaskID, attemptID))
		})
	}
	return &Client{
		store:         store,
		cred:          cred,
		registry:      registry,
		queue:         queue,
		logger:        logger,
		opts:          opts,
		assignCh:      make(chan model.Assignment, opts.LocalConcurrency+2),
		connDone:      make(chan struct{}),
		readyNotify:   make(chan struct{}, 1),
		refreshNotify: make(chan struct{}, 1),
		resultNotify:  make(chan struct{}, 1),
	}
}

// responseStream 关联一个 requestId 的响应序列。
// 通道从不关闭：等待方通过 session.done 感知断线，避免与 readLoop 的投递竞争
// 造成 send on closed channel。
type responseStream struct {
	ch chan *protocol.Envelope
}

// session 是一次 WSS 连接的全部状态；重连会创建全新 session，旧回复不会污染新会话。
type session struct {
	conn    *websocket.Conn
	sendCh  chan []byte
	done    chan struct{}
	once    sync.Once
	err     atomic.Value
	pending map[string]*responseStream
	mu      sync.Mutex
	// initialAuth 承载服务端在建连后主动下发的 AUTH_CHALLENGE。
	initialAuth chan *protocol.Envelope
	offset      atomic.Int64
	// ready 在认证 + RESUME 完成后关闭，业务发送前必须等待它。
	ready     chan struct{}
	readyOnce sync.Once
}

func newSession(conn *websocket.Conn) *session {
	return &session{
		conn:        conn,
		sendCh:      make(chan []byte, 256),
		done:        make(chan struct{}),
		pending:     map[string]*responseStream{},
		initialAuth: make(chan *protocol.Envelope, 1),
		ready:       make(chan struct{}),
	}
}

// markReady 在认证与 RESUME 完成后发布当前会话可承载业务。
func (s *session) markReady() { s.readyOnce.Do(func() { close(s.ready) }) }

// isReady 返回会话是否已完成认证与恢复。
func (s *session) isReady() bool {
	select {
	case <-s.ready:
		return true
	default:
		return false
	}
}

func (s *session) close(err error) {
	s.once.Do(func() {
		if err != nil {
			s.err.Store(err)
		} else {
			s.err.Store(ErrDisconnected)
		}
		close(s.done)
		// 只清空 pending，不关闭通道；等待方由 done 唤醒。
		s.mu.Lock()
		s.pending = map[string]*responseStream{}
		s.mu.Unlock()
	})
}

func (s *session) errValue() error {
	if v := s.err.Load(); v != nil {
		return v.(error)
	}
	return ErrDisconnected
}

func (s *session) register(reqID string) *responseStream {
	stream := &responseStream{ch: make(chan *protocol.Envelope, 4)}
	s.mu.Lock()
	s.pending[reqID] = stream
	s.mu.Unlock()
	return stream
}

func (s *session) unregister(reqID string) {
	s.mu.Lock()
	delete(s.pending, reqID)
	s.mu.Unlock()
}

func (s *session) lookup(reqID string) *responseStream {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pending[reqID]
}

// deliver 在锁内解析 requestId 后用非阻塞发送投递响应，绝不阻塞唯一 reader。
// 返回 false 表示该 requestId 已无等待方，由调用方按无关联消息处理。
func (s *session) deliver(reqID string, env *protocol.Envelope) bool {
	s.mu.Lock()
	stream := s.pending[reqID]
	s.mu.Unlock()
	if stream == nil {
		return false
	}
	select {
	case stream.ch <- env:
	default:
		// 等待方已离开或缓冲区满；丢弃不会破坏协议，等待方会超时/断线处理。
	}
	return true
}

func (c *Client) currentSession() *session {
	return c.cur.Load()
}

// activeSession 只返回已完成认证 + RESUME 的会话，业务发送必须使用它。
func (c *Client) activeSession() *session {
	if s := c.cur.Load(); s != nil && s.isReady() {
		return s
	}
	return nil
}

// ServerNow 返回按服务端 offset 校正后的时间。
func (c *Client) ServerNow() time.Time {
	return c.opts.Now().Add(time.Duration(c.offset.Load()) * time.Millisecond)
}

// Claim 由 worker 调用：阻塞等待下一条 TASK_ASSIGN，直到 ctx 取消或连接断开。
func (c *Client) Claim(ctx context.Context) (*model.Assignment, error) {
	select {
	case assignment := <-c.assignCh:
		return &assignment, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.connDone:
		return nil, c.connectionError()
	}
}

func (c *Client) connectionError() error {
	if s := c.currentSession(); s != nil {
		if err := s.errValue(); err != nil {
			return err
		}
	}
	return ErrDisconnected
}

// Credential 返回短期授权凭证。
func (c *Client) Credential() *auth.Credential { return c.cred }

// Registry 返回在途任务注册表。
func (c *Client) Registry() *tasks.Registry { return c.registry }

// Epoch 返回当前 sessionEpoch。
func (c *Client) Epoch() int64 { return c.epoch.Load() }

// HasSession 返回当前是否有活跃 WSS 会话。
func (c *Client) HasSession() bool { return c.currentSession() != nil }

// SetDraining 标记节点进入本地排空：停止 READY，心跳携带 draining。
// 本地停止（SIGTERM / WebUI 停止）不可逆，任何远端 ACTIVE 都不能撤销。
func (c *Client) SetDraining() {
	c.localDraining.Store(true)
}

// setRemoteDraining 应用服务端权威排空状态；只改远端标志，绝不撤销本地停止。
func (c *Client) setRemoteDraining(draining bool) {
	c.remoteDraining.Store(draining)
}

// Draining 返回有效排空状态：本地停止或远端排空任一为真即排空。
func (c *Client) Draining() bool {
	return c.localDraining.Load() || c.remoteDraining.Load()
}

// TriggerRefresh 请求下一次续授权（轮换切换 key 后使用）。
func (c *Client) TriggerRefresh() {
	select {
	case c.refreshNotify <- struct{}{}:
	default:
	}
}

func (c *Client) notifyReady() {
	select {
	case c.readyNotify <- struct{}{}:
	default:
	}
}

func (c *Client) notifyResult() {
	select {
	case c.resultNotify <- struct{}{}:
	default:
	}
}

// sendRaw 把已序列化帧推入当前会话的 bounded 写队列。
func (c *Client) sendRaw(s *session, data []byte, limit int) error {
	if len(data) > limit {
		return errFrameTooLarge
	}
	select {
	case s.sendCh <- data:
		return nil
	case <-s.done:
		return s.errValue()
	case <-time.After(c.opts.WriteTimeout):
		return errWriteQueueFull
	}
}

// send 组装信封并发送。payload 为 nil 时发送空对象。
func (c *Client) send(s *session, msgType, reqID string, payload any, limit int) error {
	raw := []byte("{}")
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		raw = encoded
	}
	env := protocol.Envelope{
		Version:   protocol.Version,
		Type:      msgType,
		RequestID: reqID,
		Timestamp: c.ServerNow().UTC().UnixMilli(),
		Payload:   raw,
	}
	data, err := json.Marshal(env)
	if err != nil {
		return err
	}
	return c.sendRaw(s, data, limit)
}

// request 发送请求并等待同 requestId 的响应。
func (c *Client) request(ctx context.Context, s *session, msgType string, payload any, limit int) (*protocol.Envelope, error) {
	reqID := randomRequestID()
	stream := s.register(reqID)
	defer s.unregister(reqID)
	if err := c.send(s, msgType, reqID, payload, limit); err != nil {
		return nil, err
	}
	deadline := time.NewTimer(c.opts.RequestTimeout)
	defer deadline.Stop()
	select {
	case env, ok := <-stream.ch:
		if !ok {
			return nil, s.errValue()
		}
		if env.Type == protocol.TypeError {
			return nil, parseServerError(env)
		}
		return env, nil
	case <-deadline.C:
		return nil, fmt.Errorf("wss request %s timed out", msgType)
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.done:
		return nil, s.errValue()
	}
}

func parseServerError(env *protocol.Envelope) error {
	var payload ErrorPayload
	_ = json.Unmarshal(env.Payload, &payload)
	message := strings.TrimSpace(payload.Message)
	if message == "" {
		message = "server error"
	}
	if !payload.Retryable {
		return &auth.PermanentError{Code: payload.Code, Msg: message}
	}
	return fmt.Errorf("server error code=%d: %s", payload.Code, message)
}

// readLoop 是唯一的 reader：校验帧大小/版本/类型/时间戳并分发。
func (c *Client) readLoop(s *session) {
	// 读取上限取任务帧上限，随后按类型再施加更小的控制帧上限。
	maxRead := c.opts.TaskFrameBytes
	if c.opts.ControlFrameBytes > maxRead {
		maxRead = c.opts.ControlFrameBytes
	}
	s.conn.SetReadLimit(int64(maxRead))
	defer s.close(nil)
	for {
		_, data, err := s.conn.ReadMessage()
		if err != nil {
			s.close(err)
			c.onDisconnect(s)
			return
		}
		if len(data) > maxRead {
			s.close(errFrameTooLarge)
			c.onDisconnect(s)
			return
		}
		var env protocol.Envelope
		if err := json.Unmarshal(data, &env); err != nil {
			s.close(fmt.Errorf("malformed wss envelope: %w", err))
			c.onDisconnect(s)
			return
		}
		if len(data) > c.frameLimitFor(env.Type) {
			s.close(errFrameTooLarge)
			c.onDisconnect(s)
			return
		}
		c.observeServerTime(s, &env)
		now := c.opts.Now().UTC().UnixMilli()
		if err := protocol.ValidateInbound(&env, now, c.offset.Load(), protocol.TimestampSkew.Milliseconds()); err != nil {
			// 未知版本/类型/时间戳异常一律拒绝并关闭，避免协议漂移。
			s.close(err)
			c.onDisconnect(s)
			return
		}
		if s.deliver(env.RequestID, &env) {
			continue
		}
		c.dispatch(s, &env)
	}
}

// frameLimitFor 依据合同对入站帧按类型施加 64KiB 控制 / 4MiB 任务上限。
func (c *Client) frameLimitFor(msgType string) int {
	switch msgType {
	case protocol.TypeTaskAssign, protocol.TypeTaskResult, protocol.TypeTaskRunning:
		return c.opts.TaskFrameBytes
	default:
		return c.opts.ControlFrameBytes
	}
}

// observeServerTime 用服务端时间校准 client 级偏移，入站校验与所有出站/截止判断共用。
func (c *Client) observeServerTime(s *session, env *protocol.Envelope) {
	switch env.Type {
	case protocol.TypeAuthChallenge:
		var payload ChallengePayload
		if json.Unmarshal(env.Payload, &payload) == nil && payload.ServerTime > 0 {
			c.setServerTime(payload.ServerTime)
		}
	case protocol.TypeAuthOK:
		var payload AuthOKPayload
		if json.Unmarshal(env.Payload, &payload) == nil && payload.ServerTime > 0 {
			c.setServerTime(payload.ServerTime)
		}
	}
}

func (c *Client) setServerTime(serverTimeMillis int64) {
	offset := serverTimeMillis - c.opts.Now().UTC().UnixMilli()
	c.offset.Store(offset)
	if s := c.currentSession(); s != nil {
		s.offset.Store(offset)
	}
}

// dispatch 处理无 pending 关联的服务端消息。
func (c *Client) dispatch(s *session, env *protocol.Envelope) {
	switch env.Type {
	case protocol.TypeAuthChallenge:
		select {
		case s.initialAuth <- env:
		default:
			c.logger.Warn("unexpected extra AUTH_CHALLENGE")
		}
	case protocol.TypeTaskAssign:
		c.handleAssign(s, env)
	case protocol.TypeTaskCancel:
		c.handleCancel(env)
	case protocol.TypeNodeDrain:
		// 服务端发起的 NODE_DRAIN 属远端排空，可由后续权威 ACTIVE 恢复。
		c.setRemoteDraining(true)
		c.notifyReady()
	case protocol.TypeNodeState:
		c.handleNodeState(env)
	case protocol.TypePing:
		_ = c.send(s, protocol.TypePong, env.RequestID, nil, c.opts.ControlFrameBytes)
	case protocol.TypeError:
		var payload ErrorPayload
		_ = json.Unmarshal(env.Payload, &payload)
		if !payload.Retryable {
			if strings.TrimSpace(env.RequestID) != "" {
				// 带 requestId 的永久 ERROR 只影响对应请求，不无差别吊销整个身份。
				c.logger.Warn("request-scoped permanent error",
					logging.Int("code", payload.Code), logging.String("requestId", env.RequestID))
				return
			}
			c.logger.Warn("server sent permanent session error", logging.Int("code", payload.Code))
			c.triggerRevoked(payload.Message)
		}
	default:
		c.logger.Warn("unexpected server message", logging.String("type", env.Type))
	}
}

// handleNodeState 处理服务端的身份状态权威通知：
// DISABLED/REVOKED/EXPIRED 必须立即取消在途任务，绝不继续跑到租约到期。
// DRAINING/ACTIVE 只调整可逆的远端排空标志，本地停止不受影响。
func (c *Client) handleNodeState(env *protocol.Envelope) {
	var payload NodeStatePayload
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		c.logger.Warn("malformed NODE_STATE ignored")
		return
	}
	switch strings.ToUpper(strings.TrimSpace(payload.Status)) {
	case "DISABLED", "REVOKED", "EXPIRED":
		c.logger.Warn("server declared node identity inactive",
			logging.String("status", payload.Status))
		c.triggerRevoked("node status " + payload.Status)
	case "DRAINING":
		c.setRemoteDraining(true)
		c.notifyReady()
	case "ACTIVE":
		// 只有明确的 ACTIVE 且 draining=false 才清除远端排空；
		// ACTIVE 却带 draining=true 属不一致信号，按更保守的排空处理，绝不恢复。
		if payload.Draining {
			c.setRemoteDraining(true)
		} else {
			c.setRemoteDraining(false)
		}
		c.notifyReady()
	default:
		// 未知/非法状态不改变任何排空标志，绝不恢复。
		c.logger.Warn("unknown NODE_STATE status ignored",
			logging.String("status", payload.Status))
	}
}

func (c *Client) handleAssign(s *session, env *protocol.Envelope) {
	var payload TaskAssignPayload
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		c.logger.Warn("malformed TASK_ASSIGN ignored")
		return
	}
	if payload.Task.SubmissionID == "" || payload.Task.JudgeTaskID == "" || payload.AttemptID == "" || payload.LeaseUntil <= 0 {
		c.logger.Warn("TASK_ASSIGN missing required fields")
		return
	}
	if !s.isReady() {
		c.logger.Warn("TASK_ASSIGN before auth/resume complete, ignored")
		return
	}
	// 已接纳（在途或最近完成）的同 attempt 必须先于排空/额度/队列判断，只 ACK：
	// 重复分配绝不重复执行，也绝不再占一份容量或额度。
	if c.registry.Has(payload.Task.SubmissionID, payload.Task.JudgeTaskID, payload.AttemptID) {
		c.ackTask(s, env.RequestID, payload.Task.SubmissionID, payload.Task.JudgeTaskID, payload.AttemptID)
		return
	}
	if c.Draining() {
		c.logger.Warn("TASK_ASSIGN while draining, rejected")
		return
	}
	if !c.modeAllowed(payload.Task.JudgeMode) {
		c.logger.Warn("TASK_ASSIGN with unsupported judge mode", logging.String("mode", payload.Task.JudgeMode))
		c.failConnection(s, errAssignOverflow)
		return
	}
	if c.registry.Len() >= c.approvedSlots() {
		c.logger.Warn("TASK_ASSIGN exceeds approved concurrency, closing connection")
		c.failConnection(s, errAssignOverflow)
		return
	}
	// 预留该 attempt 的终态结果容量；队列满/磁盘降级时绝不接纳，避免结果无处持久化。
	key := taskKey(payload.Task.SubmissionID, payload.Task.JudgeTaskID, payload.AttemptID)
	if err := c.reserveResultCapacity(key); err != nil {
		c.logger.Warn("TASK_ASSIGN rejected: durable result capacity unavailable", logging.Error(err))
		c.rejectAssign(s, env.RequestID, "result capacity unavailable")
		return
	}
	payload.Task.AttemptID = payload.AttemptID
	ctx, cancel := context.WithCancel(c.rootContext())
	attempt := tasks.Attempt{
		SubmissionID: payload.Task.SubmissionID,
		JudgeTaskID:  payload.Task.JudgeTaskID,
		AttemptID:    payload.AttemptID,
		LeaseUntil:   payload.LeaseUntil,
		Epoch:        c.epoch.Load(),
	}
	if !c.registry.Start(attempt, cancel) {
		cancel()
		c.releaseResultCapacity(key)
		c.ackTask(s, env.RequestID, payload.Task.SubmissionID, payload.Task.JudgeTaskID, payload.AttemptID)
		return
	}
	assignment := model.Assignment{
		Task:             payload.Task,
		AttemptID:        payload.AttemptID,
		LeaseUntil:       payload.LeaseUntil,
		RenewAfterMillis: payload.RenewAfterMillis,
		Ctx:              ctx,
	}
	select {
	case c.assignCh <- assignment:
		c.ackTask(s, env.RequestID, payload.Task.SubmissionID, payload.Task.JudgeTaskID, payload.AttemptID)
	case <-s.done:
		cancel()
		c.registry.Complete(payload.Task.SubmissionID, payload.Task.JudgeTaskID, payload.AttemptID)
	default:
		// 超出批准额度：取消本地 attempt 并断开，服务端会凭租约重新分配。
		cancel()
		c.registry.Complete(payload.Task.SubmissionID, payload.Task.JudgeTaskID, payload.AttemptID)
		c.failConnection(s, errAssignOverflow)
	}
}

// reserveResultCapacity 在接纳 attempt 前预留其终态结果的持久容量。
func (c *Client) reserveResultCapacity(key string) error {
	if c.queue == nil {
		return nil
	}
	return c.queue.Reserve(key)
}

// releaseResultCapacity 释放未被终态消费的预留容量。
func (c *Client) releaseResultCapacity(key string) {
	if c.queue != nil {
		c.queue.Release(key)
	}
}

// rejectAssign 用可重试 ERROR 回绝本节点当前无法接纳的分配：服务端按 requestId 关联，
// 保留租约并可稍后重投，而不是把任务静默丢弃。
func (c *Client) rejectAssign(s *session, requestID, reason string) {
	if strings.TrimSpace(requestID) == "" {
		return
	}
	if err := c.send(s, protocol.TypeError, requestID, ErrorPayload{Code: 503, Message: reason, Retryable: true}, c.opts.ControlFrameBytes); err != nil {
		c.logger.Warn("failed to reject TASK_ASSIGN", logging.Error(err))
	}
}

func (c *Client) failConnection(s *session, err error) {
	c.logger.Warn("closing wss connection", logging.Error(err))
	s.close(err)
	c.onDisconnect(s)
}

// approvedSlots 返回服务端批准与本地配置双重约束后的并发上限。
func (c *Client) approvedSlots() int {
	approved := c.cred.Snapshot().MaxConcurrency
	if approved <= 0 || approved > c.opts.LocalConcurrency {
		approved = c.opts.LocalConcurrency
	}
	if approved <= 0 {
		approved = 1
	}
	return approved
}

// modeAllowed 校验服务端下发的判题模式确实在本地/批准集合内。
func (c *Client) modeAllowed(mode string) bool {
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "" {
		return true
	}
	allowed := c.cred.Snapshot().SupportedJudgeModes
	if len(allowed) == 0 {
		allowed = c.opts.SupportedJudgeModes
	}
	if len(allowed) == 0 {
		return true
	}
	for _, candidate := range allowed {
		if strings.EqualFold(strings.TrimSpace(candidate), mode) {
			return true
		}
	}
	return false
}

func (c *Client) handleCancel(env *protocol.Envelope) {
	var payload TaskCancelPayload
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		c.logger.Warn("malformed TASK_CANCEL ignored")
		return
	}
	for _, item := range payload.Tasks {
		if c.registry.Cancel(item.SubmissionID, item.JudgeTaskID, item.AttemptID) {
			c.logger.Info("task cancelled by server",
				logging.String("submissionId", item.SubmissionID), logging.String("reason", payload.Reason))
		}
	}
}

// ackTask 用收到 TASK_ASSIGN 的 requestId 回 ACK，保持每个 reply 的关联。
func (c *Client) ackTask(s *session, requestID, submissionID, judgeTaskID, attemptID string) {
	if requestID == "" {
		requestID = randomRequestID()
	}
	if err := c.send(s, protocol.TypeTaskAck, requestID, TaskAckPayload{
		SubmissionID: submissionID, JudgeTaskID: judgeTaskID, AttemptID: attemptID,
	}, c.opts.ControlFrameBytes); err != nil {
		c.logger.Warn("failed to ACK task", logging.Error(err))
	}
}

func (c *Client) triggerRevoked(reason string) {
	c.revokeOnce.Do(func() {
		if c.opts.OnRevoked != nil {
			c.opts.OnRevoked(reason)
		}
	})
}

func (c *Client) rootContext() context.Context {
	c.rootMu.Lock()
	defer c.rootMu.Unlock()
	if c.rootCtx == nil {
		return context.Background()
	}
	return c.rootCtx
}

func randomRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("req-%d", time.Now().UnixNano())
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}

// ValidateWSSURL 要求远程必须 wss；ws 只允许明确 loopback 开发地址。
func ValidateWSSURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("wss url invalid: %w", err)
	}
	if u.Host == "" {
		return fmt.Errorf("wss url must be absolute, got %q", raw)
	}
	switch strings.ToLower(u.Scheme) {
	case "wss", "https":
		return nil
	case "ws", "http":
		host := strings.ToLower(u.Hostname())
		if host == "localhost" || host == "127.0.0.1" || host == "::1" || host == "[::1]" {
			return nil
		}
		return fmt.Errorf("wss url must use wss for non-loopback hosts, got %q", raw)
	default:
		return fmt.Errorf("wss url must use ws or wss, got %q", raw)
	}
}
