package mq

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"

	"github.com/criyle/go-judge/internal/hnieoj/config"
	"github.com/criyle/go-judge/internal/hnieoj/logging"
	"github.com/criyle/go-judge/internal/hnieoj/model"
	"github.com/criyle/go-judge/internal/hnieoj/processor"
	amqp "github.com/rabbitmq/amqp091-go"
)

type Consumer struct {
	cfg    config.RabbitMQConfig
	logger logging.Logger
}

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
				c.handleDelivery(ctx, d, handler)
			}()
		}
	}
}

func (c *Consumer) handleDelivery(ctx context.Context, d amqp.Delivery, handler func(context.Context, model.Task) error) {
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
			_ = d.Nack(false, true)
			return
		}
		c.logger.Warn("task failed with non-retryable error", logging.String("submissionId", task.SubmissionID), logging.Error(err))
		_ = d.Ack(false)
		return
	}
	_ = d.Ack(false)
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
