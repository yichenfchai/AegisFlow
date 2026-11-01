package deployments

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"sync"
	"time"
)

const (
	// maxDeployEventBytes truncates one response line (rune boundary aware).
	maxDeployEventBytes = 4096
	// maxDeployResponseEvents caps stored upstream lines per deployment; one
	// system line marks the truncation point. Lifecycle system lines are not
	// counted so a noisy upstream cannot hide them.
	maxDeployResponseEvents = 5000
)

// deploymentEventWriter batches events and caps upstream output. Writes happen
// from the executing goroutine; flush persists batches from its own loop.
type deploymentEventWriter struct {
	s            *Service
	deploymentID string
	done         chan struct{}
	mu           sync.Mutex
	pending      []DeploymentEvent
	response     int
	truncated    bool
}

func newDeploymentEventWriter(s *Service, deploymentID string) *deploymentEventWriter {
	w := &deploymentEventWriter{s: s, deploymentID: deploymentID, done: make(chan struct{})}
	go w.flushLoop()
	return w
}

func (w *deploymentEventWriter) flushLoop() {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-w.done:
			return
		case <-ticker.C:
			w.flush()
		}
	}
}

// stream stores each upstream line as a response event. A line past the
// scanner buffer is still bounded by maxDeployEventBytes truncation; the
// scanner error is returned so the caller can record why reading stopped.
func (w *deploymentEventWriter) stream(reader io.Reader) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*maxDeployEventBytes)
	for scanner.Scan() {
		w.append("response", scanner.Text())
	}
	return scanner.Err()
}

func (w *deploymentEventWriter) systemf(format string, args ...any) {
	w.append("system", fmt.Sprintf(format, args...))
}

func (w *deploymentEventWriter) append(stream, message string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if stream == "response" {
		if w.response >= maxDeployResponseEvents {
			if !w.truncated {
				w.truncated = true
				w.pending = append(w.pending, DeploymentEvent{DeploymentID: w.deploymentID, Stream: "system", Message: "response truncated: output past the event cap is dropped", CreatedAt: time.Now().Unix()})
			}
			return
		}
		w.response++
		if len(message) > maxDeployEventBytes {
			message = truncateEventBytes(message, maxDeployEventBytes)
		}
	}
	w.pending = append(w.pending, DeploymentEvent{DeploymentID: w.deploymentID, Stream: stream, Message: message, CreatedAt: time.Now().Unix()})
}

func (w *deploymentEventWriter) flush() {
	w.mu.Lock()
	batch := w.pending
	w.pending = nil
	w.mu.Unlock()
	if len(batch) == 0 {
		return
	}
	// Log storage hiccups must not fail a deployment whose upstream call
	// already succeeded; events are best effort within the documented cap.
	_ = w.s.appendDeploymentEvents(batch)
}

func (w *deploymentEventWriter) close() {
	close(w.done)
	w.flush()
}

func (s *Service) appendDeploymentEvents(events []DeploymentEvent) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, event := range events {
		if _, err = tx.ExecContext(ctx, `INSERT INTO deployment_events(deployment_id,stream,message,created_at) VALUES($1,$2,$3,$4)`,
			event.DeploymentID, event.Stream, event.Message, event.CreatedAt); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// truncateEventBytes cuts at the byte limit on a rune boundary.
func truncateEventBytes(value string, limit int) string {
	cut := value[:limit]
	for len(cut) > 0 && cut[len(cut)-1]&0xC0 == 0x80 {
		cut = cut[:len(cut)-1]
	}
	return cut + " …[truncated]"
}
