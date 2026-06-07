package mq

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/criyle/go-judge/internal/hnieoj/config"
	"github.com/criyle/go-judge/internal/hnieoj/logging"
	"github.com/criyle/go-judge/internal/hnieoj/model"
	"github.com/criyle/go-judge/internal/hnieoj/processor"
	amqp "github.com/rabbitmq/amqp091-go"
)

type Consumer struct {
	cfg       config.RabbitMQConfig
	logger    logging.Logger
	publishMu sync.Mutex
}

const retryCountHeader = "x-hnieoj-retry-count"

func New(cfg config.RabbitMQConfig, logger logging.Logger) *Consumer {
	return &Consumer{cfg: cfg, logger: logger}
}

func (c *Consumer) Consume(ctx context.Context, handler func(context.Context, model.Task) error) error {
	conn, err := amqp.Dial(c.dsn())
	if err != nil {
		return err
	}
	defer conn.Close()

	ch, err := conn.Channel()
	if err != nil {
		return err
	}
	defer ch.Close()

	if err := ch.ExchangeDeclare(c.cfg.Exchange, "direct", true, false, false, false, nil); err != nil {
		return err
	}
	if c.cfg.DeadLetterExchange != "" {
		if err := ch.ExchangeDeclare(c.cfg.DeadLetterExchange, "direct", true, false, false, false, nil); err != nil {
			return err
		}
	}
	if c.cfg.DeadLetterQueue != "" {
		if _, err := ch.QueueDeclare(c.cfg.DeadLetterQueue, true, false, false, false, nil); err != nil {
			return err
		}
		if c.cfg.DeadLetterExchange != "" && c.cfg.DeadLetterRoutingKey != "" {
			if err := ch.QueueBind(c.cfg.DeadLetterQueue, c.cfg.DeadLetterRoutingKey, c.cfg.DeadLetterExchange, false, nil); err != nil {
				return err
			}
		}
	}
	queueArgs := amqp.Table{}
	if c.cfg.DeadLetterExchange != "" {
		queueArgs["x-dead-letter-exchange"] = c.cfg.DeadLetterExchange
	}
	if c.cfg.DeadLetterRoutingKey != "" {
		queueArgs["x-dead-letter-routing-key"] = c.cfg.DeadLetterRoutingKey
	}
	q, err := ch.QueueDeclare(c.cfg.Queue, true, false, false, false, queueArgs)
	if err != nil {
		return err
	}
	if err := ch.QueueBind(q.Name, c.cfg.RoutingKey, c.cfg.Exchange, false, nil); err != nil {
		return err
	}
	if err := ch.Qos(c.cfg.Prefetch, 0, false); err != nil {
		return err
	}

	deliveries, err := ch.Consume(q.Name, "", false, false, false, false, nil)
	if err != nil {
		return err
	}
	c.logger.Info("rabbitmq consumer started", logging.String("queue", q.Name), logging.String("exchange", c.cfg.Exchange), logging.String("routingKey", c.cfg.RoutingKey))
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case d, ok := <-deliveries:
			if !ok {
				return errors.New("rabbitmq delivery channel closed")
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				c.handleDelivery(ctx, ch, d, handler)
			}()
		}
	}
}

func (c *Consumer) handleDelivery(ctx context.Context, ch *amqp.Channel, d amqp.Delivery, handler func(context.Context, model.Task) error) {
	var task model.Task
	if err := json.Unmarshal(d.Body, &task); err != nil {
		c.logger.Warn("invalid task message", logging.Error(err))
		_ = d.Ack(false)
		return
	}
	if err := handler(ctx, task); err != nil {
		var retryable processor.ErrRetryable
		if errors.As(err, &retryable) {
			c.logger.Warn("task failed with retryable error", logging.String("submissionId", task.SubmissionID), logging.Error(err))
			c.retryOrDeadLetter(ctx, ch, d, task, err)
			return
		}
		c.logger.Warn("task failed with non-retryable error", logging.String("submissionId", task.SubmissionID), logging.Error(err))
		_ = d.Ack(false)
		return
	}
	_ = d.Ack(false)
}

func (c *Consumer) retryOrDeadLetter(ctx context.Context, ch *amqp.Channel, d amqp.Delivery, task model.Task, err error) {
	retryCount := readRetryCount(d.Headers)
	if retryCount >= c.cfg.MaxRetries {
		c.logger.Warn("task exceeded retry limit, dead-lettering",
			logging.String("submissionId", task.SubmissionID),
			logging.Int("retryCount", retryCount),
			logging.Error(err))
		_ = d.Nack(false, false)
		return
	}

	nextRetryCount := retryCount + 1
	backoff := c.cfg.RetryBackoff
	if backoff <= 0 {
		backoff = 10 * time.Second
	}
	timer := time.NewTimer(backoff)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		_ = d.Nack(false, true)
		return
	case <-timer.C:
	}

	headers := cloneHeaders(d.Headers)
	headers[retryCountHeader] = nextRetryCount
	publishing := amqp.Publishing{
		Headers:         headers,
		ContentType:     defaultString(d.ContentType, "application/json"),
		ContentEncoding: d.ContentEncoding,
		DeliveryMode:    amqp.Persistent,
		Priority:        d.Priority,
		CorrelationId:   d.CorrelationId,
		ReplyTo:         d.ReplyTo,
		Expiration:      d.Expiration,
		MessageId:       d.MessageId,
		Timestamp:       time.Now(),
		Type:            d.Type,
		UserId:          d.UserId,
		AppId:           d.AppId,
		Body:            d.Body,
	}
	c.publishMu.Lock()
	err = ch.PublishWithContext(ctx, c.cfg.Exchange, c.cfg.RoutingKey, false, false, publishing)
	c.publishMu.Unlock()
	if err != nil {
		c.logger.Warn("republish retry task failed, dead-lettering",
			logging.String("submissionId", task.SubmissionID),
			logging.Int("retryCount", nextRetryCount),
			logging.Error(err))
		_ = d.Nack(false, false)
		return
	}
	c.logger.Warn("task scheduled for retry",
		logging.String("submissionId", task.SubmissionID),
		logging.Int("retryCount", nextRetryCount),
		logging.String("backoff", backoff.String()))
	_ = d.Ack(false)
}

func readRetryCount(headers amqp.Table) int {
	if headers == nil {
		return 0
	}
	value, ok := headers[retryCountHeader]
	if !ok {
		return 0
	}
	switch item := value.(type) {
	case int:
		return item
	case int8:
		return int(item)
	case int16:
		return int(item)
	case int32:
		return int(item)
	case int64:
		return int(item)
	case uint:
		return int(item)
	case uint8:
		return int(item)
	case uint16:
		return int(item)
	case uint32:
		return int(item)
	case uint64:
		return int(item)
	case float32:
		return int(item)
	case float64:
		return int(item)
	default:
		return 0
	}
}

func cloneHeaders(headers amqp.Table) amqp.Table {
	copied := amqp.Table{}
	for key, value := range headers {
		copied[key] = value
	}
	return copied
}

func defaultString(value, defaultValue string) string {
	if value == "" {
		return defaultValue
	}
	return value
}

func (c *Consumer) dsn() string {
	user := url.QueryEscape(c.cfg.Username)
	pass := url.QueryEscape(c.cfg.Password)
	vhost := c.cfg.VirtualHost
	if vhost == "" {
		vhost = "/"
	}
	if vhost == "/" {
		vhost = "%2F"
	} else {
		vhost = url.PathEscape(strings.TrimPrefix(vhost, "/"))
	}
	return fmt.Sprintf("amqp://%s:%s@%s:%d/%s", user, pass, c.cfg.Host, c.cfg.Port, vhost)
}
