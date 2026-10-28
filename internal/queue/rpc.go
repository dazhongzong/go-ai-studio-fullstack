package queue

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

const DefaultJobQueue = "inference.jobs"

// CancelExchange is a fanout exchange. Every inference worker receives every
// cancellation notification and only the worker that owns the task acts on
// it. This is necessary because the API does not know which worker consumed a
// job from the shared job queue.
const CancelExchange = "inference.cancels"

var ErrClosed = errors.New("queue: client closed")

type job struct {
	Prompt string `json:"prompt"`
}

type result struct {
	Output string `json:"output,omitempty"`
	Error  string `json:"error,omitempty"`
}

type Client struct {
	connection *amqp.Connection
	publisher  *amqp.Channel
	replies    *amqp.Channel
	replyQueue string

	publishMu sync.Mutex
	pending   sync.Map // correlation ID -> chan result
	done      chan struct{}
	closeOnce sync.Once
}

// Ready reports whether the RabbitMQ connection is still usable.
func (c *Client) Ready() error {
	if c == nil || c.connection == nil || c.connection.IsClosed() {
		return ErrClosed
	}
	return nil
}

// NewClient creates a RabbitMQ request/reply client. Multiple API instances
// can use the same job queue and each instance gets its own private reply
// queue, so a load balancer does not need sticky sessions for /ask.
func NewClient(url string) (*Client, error) {
	connection, err := amqp.Dial(url)
	if err != nil {
		return nil, fmt.Errorf("dial rabbitmq: %w", err)
	}
	publisher, err := connection.Channel()
	if err != nil {
		_ = connection.Close()
		return nil, fmt.Errorf("open rabbitmq publisher: %w", err)
	}
	replies, err := connection.Channel()
	if err != nil {
		_ = publisher.Close()
		_ = connection.Close()
		return nil, fmt.Errorf("open rabbitmq reply channel: %w", err)
	}
	if _, err := publisher.QueueDeclare(DefaultJobQueue, true, false, false, false, nil); err != nil {
		_ = replies.Close()
		_ = publisher.Close()
		_ = connection.Close()
		return nil, fmt.Errorf("declare rabbitmq job queue: %w", err)
	}
	if err := publisher.ExchangeDeclare(CancelExchange, amqp.ExchangeFanout, true, false, false, false, nil); err != nil {
		_ = replies.Close()
		_ = publisher.Close()
		_ = connection.Close()
		return nil, fmt.Errorf("declare rabbitmq cancellation exchange: %w", err)
	}
	replyQueue, err := replies.QueueDeclare("", false, true, true, false, nil)
	if err != nil {
		_ = replies.Close()
		_ = publisher.Close()
		_ = connection.Close()
		return nil, fmt.Errorf("declare rabbitmq reply queue: %w", err)
	}
	deliveries, err := replies.Consume(replyQueue.Name, "", true, true, false, false, nil)
	if err != nil {
		_ = replies.Close()
		_ = publisher.Close()
		_ = connection.Close()
		return nil, fmt.Errorf("consume rabbitmq replies: %w", err)
	}

	client := &Client{
		connection: connection,
		publisher:  publisher,
		replies:    replies,
		replyQueue: replyQueue.Name,
		done:       make(chan struct{}),
	}
	go client.dispatch(deliveries)
	log.Printf("rabbitmq client ready job_queue=%s reply_queue=%s", DefaultJobQueue, replyQueue.Name)
	return client, nil
}

func (c *Client) dispatch(deliveries <-chan amqp.Delivery) {
	for {
		select {
		case <-c.done:
			return
		case delivery, ok := <-deliveries:
			if !ok {
				return
			}
			value, ok := c.pending.LoadAndDelete(delivery.CorrelationId)
			if !ok {
				continue
			}
			var response result
			if err := json.Unmarshal(delivery.Body, &response); err != nil {
				response.Error = fmt.Sprintf("invalid queue response: %v", err)
			}
			value.(chan result) <- response
		}
	}
}

func (c *Client) Call(ctx context.Context, prompt string) (string, error) {
	if ctx == nil {
		return "", errors.New("queue: nil context")
	}
	select {
	case <-c.done:
		return "", ErrClosed
	default:
	}

	correlationID, err := randomID()
	if err != nil {
		return "", err
	}
	startedAt := time.Now()
	responseCh := make(chan result, 1)
	c.pending.Store(correlationID, responseCh)
	defer c.pending.Delete(correlationID)

	body, err := json.Marshal(job{Prompt: prompt})
	if err != nil {
		return "", err
	}
	c.publishMu.Lock()
	select {
	case <-c.done:
		c.publishMu.Unlock()
		return "", ErrClosed
	default:
	}
	err = c.publisher.PublishWithContext(ctx, "", DefaultJobQueue, false, false, amqp.Publishing{
		ContentType:   "application/json",
		CorrelationId: correlationID,
		ReplyTo:       c.replyQueue,
		DeliveryMode:  amqp.Persistent,
		Body:          body,
	})
	c.publishMu.Unlock()
	if err != nil {
		return "", fmt.Errorf("publish inference job: %w", err)
	}
	log.Printf("queue task published task_id=%s prompt_length=%d", correlationID, len(prompt))

	select {
	case response := <-responseCh:
		if response.Error != "" {
			log.Printf("queue task failed task_id=%s duration=%s error=%s", correlationID, time.Since(startedAt), response.Error)
			return "", errors.New(response.Error)
		}
		log.Printf("queue task completed task_id=%s duration=%s", correlationID, time.Since(startedAt))
		return response.Output, nil
	case <-ctx.Done():
		// The job may already have been consumed by an inference worker. A
		// context cancellation on this API process cannot reach that worker,
		// so send an explicit best-effort cancellation notification.
		if _, stillPending := c.pending.Load(correlationID); stillPending {
			c.publishCancel(correlationID)
		}
		log.Printf("queue task canceled task_id=%s duration=%s error=%v", correlationID, time.Since(startedAt), ctx.Err())
		return "", ctx.Err()
	case <-c.done:
		return "", ErrClosed
	}
}

func (c *Client) publishCancel(correlationID string) {
	// Cancellation must not reuse the already-canceled request context. It is
	// best effort: the API already has a terminal error to return to its caller.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c.publishMu.Lock()
	select {
	case <-c.done:
		c.publishMu.Unlock()
		return
	default:
	}
	err := c.publisher.PublishWithContext(ctx, CancelExchange, "", false, false, amqp.Publishing{
		ContentType:   "application/json",
		CorrelationId: correlationID,
		DeliveryMode:  amqp.Persistent,
		Type:          "inference.cancel",
	})
	c.publishMu.Unlock()
	if err != nil {
		log.Printf("rabbitmq: publish cancellation task_id=%s: %v", correlationID, err)
	}
}

func (c *Client) Close() error {
	var err error
	c.closeOnce.Do(func() {
		close(c.done)
		if closeErr := c.replies.Close(); closeErr != nil {
			err = closeErr
		}
		if closeErr := c.publisher.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
		if closeErr := c.connection.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
	})
	return err
}

type Handler func(context.Context, string) (string, error)

// RunWorker consumes inference jobs. Run multiple worker processes to scale
// inference independently from the HTTP API instances.
func RunWorker(ctx context.Context, url, workerID string, handler Handler) error {
	instanceID, err := randomID()
	if err != nil {
		return fmt.Errorf("generate worker instance id: %w", err)
	}
	pid := os.Getpid()
	workerLog := func(format string, args ...interface{}) {
		prefixArgs := []interface{}{workerID, pid, instanceID}
		prefixArgs = append(prefixArgs, args...)
		log.Printf("worker_id=%s pid=%d instance_id=%s "+format, prefixArgs...)
	}

	workerLog("rabbitmq worker connecting job_queue=%s", DefaultJobQueue)
	connection, err := amqp.Dial(url)
	if err != nil {
		return fmt.Errorf("dial rabbitmq: %w", err)
	}
	defer connection.Close()
	channel, err := connection.Channel()
	if err != nil {
		return fmt.Errorf("open rabbitmq worker channel: %w", err)
	}
	defer channel.Close()
	if _, err := channel.QueueDeclare(DefaultJobQueue, true, false, false, false, nil); err != nil {
		return fmt.Errorf("declare rabbitmq worker queue: %w", err)
	}
	if err := channel.ExchangeDeclare(CancelExchange, amqp.ExchangeFanout, true, false, false, false, nil); err != nil {
		return fmt.Errorf("declare rabbitmq cancellation exchange: %w", err)
	}
	if err := channel.Qos(1, 0, false); err != nil {
		return fmt.Errorf("configure rabbitmq prefetch: %w", err)
	}
	deliveries, err := channel.Consume(DefaultJobQueue, "", false, false, false, false, nil)
	if err != nil {
		return fmt.Errorf("consume rabbitmq jobs: %w", err)
	}

	// Use a separate channel for cancellation control traffic. The job loop
	// blocks while running inference, so it must be possible to receive a
	// cancellation notification concurrently.
	cancelChannel, err := connection.Channel()
	if err != nil {
		return fmt.Errorf("open rabbitmq cancellation channel: %w", err)
	}
	defer cancelChannel.Close()
	cancelQueue, err := cancelChannel.QueueDeclare("", false, true, true, false, nil)
	if err != nil {
		return fmt.Errorf("declare rabbitmq cancellation queue: %w", err)
	}
	if err := cancelChannel.QueueBind(cancelQueue.Name, "", CancelExchange, false, nil); err != nil {
		return fmt.Errorf("bind rabbitmq cancellation queue: %w", err)
	}
	cancelDeliveries, err := cancelChannel.Consume(cancelQueue.Name, "", false, true, false, false, nil)
	if err != nil {
		return fmt.Errorf("consume rabbitmq cancellations: %w", err)
	}
	workerLog("rabbitmq worker ready job_queue=%s cancel_exchange=%s prefetch=%d", DefaultJobQueue, CancelExchange, 1)

	type activeTask struct {
		cancel context.CancelFunc
	}
	var activeMu sync.Mutex
	active := make(map[string]activeTask)
	canceled := make(map[string]time.Time)
	cancelDone := make(chan struct{})
	go func() {
		defer close(cancelDone)
		cleanupTicker := time.NewTicker(time.Minute)
		defer cleanupTicker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-cleanupTicker.C:
				activeMu.Lock()
				for taskID, at := range canceled {
					if now.Sub(at) > 10*time.Minute {
						delete(canceled, taskID)
					}
				}
				activeMu.Unlock()
			case delivery, ok := <-cancelDeliveries:
				if !ok {
					return
				}
				if delivery.CorrelationId == "" {
					continue
				}
				activeMu.Lock()
				if task, ok := active[delivery.CorrelationId]; ok {
					task.cancel()
					workerLog("inference task cancellation received task_id=%s active=true", delivery.CorrelationId)
				} else {
					// The cancellation can arrive before the job delivery. Keep a
					// bounded-time tombstone so the queued job is skipped later.
					canceled[delivery.CorrelationId] = time.Now()
					workerLog("inference task cancellation received task_id=%s active=false", delivery.CorrelationId)
				}
				activeMu.Unlock()
			}
		}
	}()
	defer func() {
		_ = cancelChannel.Close()
		<-cancelDone
	}()

	for {
		select {
		case <-ctx.Done():
			return nil
		case delivery, ok := <-deliveries:
			if !ok {
				return errors.New("rabbitmq job delivery closed")
			}
			var request job
			if err := json.Unmarshal(delivery.Body, &request); err != nil {
				workerLog("inference task rejected task_id=%s error=%v", delivery.CorrelationId, err)
				_ = publishResult(channel, delivery, result{Error: err.Error()})
				_ = delivery.Ack(false)
				continue
			}

			jobCtx, cancel := context.WithCancel(ctx)
			startedAt := time.Now()
			workerLog("inference task received task_id=%s prompt_length=%d", delivery.CorrelationId, len(request.Prompt))
			activeMu.Lock()
			wasCanceled := false
			if _, ok := canceled[delivery.CorrelationId]; ok {
				wasCanceled = true
				delete(canceled, delivery.CorrelationId)
			}
			if !wasCanceled {
				active[delivery.CorrelationId] = activeTask{cancel: cancel}
			}
			activeMu.Unlock()

			var output string
			var handlerErr error
			if wasCanceled {
				cancel()
				handlerErr = context.Canceled
			} else if err := jobCtx.Err(); err != nil {
				cancel()
				handlerErr = err
			} else {
				output, handlerErr = handler(jobCtx, request.Prompt)
				activeMu.Lock()
				delete(active, delivery.CorrelationId)
				activeMu.Unlock()
				cancel()
			}
			response := result{Output: output}
			if handlerErr != nil {
				response.Output = ""
				response.Error = handlerErr.Error()
			}
			status := "success"
			if handlerErr != nil {
				status = "failed"
				if errors.Is(handlerErr, context.Canceled) || errors.Is(handlerErr, context.DeadlineExceeded) {
					status = "canceled"
				}
			}
			if err := publishResult(channel, delivery, response); err != nil {
				return err
			}
			if err := delivery.Ack(false); err != nil {
				return fmt.Errorf("ack inference job: %w", err)
			}
			workerLog("inference task completed task_id=%s status=%s duration=%s", delivery.CorrelationId, status, time.Since(startedAt))
		}
	}
}

func publishResult(channel *amqp.Channel, delivery amqp.Delivery, response result) error {
	body, err := json.Marshal(response)
	if err != nil {
		return err
	}
	return channel.PublishWithContext(context.Background(), "", delivery.ReplyTo, false, false, amqp.Publishing{
		ContentType:   "application/json",
		CorrelationId: delivery.CorrelationId,
		DeliveryMode:  amqp.Persistent,
		Body:          body,
	})
}

func randomID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate correlation id: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}
