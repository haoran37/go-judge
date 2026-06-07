package mq

import (
	"testing"

	amqp "github.com/rabbitmq/amqp091-go"
)

func TestReadRetryCountSupportsAMQPIntegerTypes(t *testing.T) {
	headers := amqp.Table{retryCountHeader: int32(3)}
	if got := readRetryCount(headers); got != 3 {
		t.Fatalf("retry count = %d, want 3", got)
	}
}

func TestCloneHeadersKeepsRetryCount(t *testing.T) {
	headers := amqp.Table{retryCountHeader: 2, "x-custom": "value"}
	copied := cloneHeaders(headers)
	copied[retryCountHeader] = 3
	if headers[retryCountHeader] != 2 {
		t.Fatalf("source headers were mutated: %#v", headers)
	}
	if copied["x-custom"] != "value" {
		t.Fatalf("custom header missing: %#v", copied)
	}
}
