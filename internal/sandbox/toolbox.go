package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/tool"
)

// QueueToolbox exposes a queue Client as a tool.Toolbox: the sandbox
// serves the builtin toolset, executed on the far side of the queue.
type QueueToolbox struct {
	Client  *Client
	Timeout time.Duration
	defs    []llm.Tool
}

// NewQueueToolbox wraps client.
func NewQueueToolbox(client *Client) *QueueToolbox {
	return &QueueToolbox{
		Client:  client,
		Timeout: 11 * time.Minute, // > bash's 10-minute cap
		defs:    tool.Builtins().Defs(),
	}
}

// Defs implements tool.Toolbox.
func (q *QueueToolbox) Defs() []llm.Tool { return q.defs }

// Run implements tool.Toolbox.
func (q *QueueToolbox) Run(ctx context.Context, name string, input json.RawMessage) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, q.Timeout)
	defer cancel()
	content, isError, err := q.Client.Exec(ctx, name, input)
	if err != nil {
		return "", err
	}
	if isError {
		return "", fmt.Errorf("%s", strings.TrimPrefix(content, "error: "))
	}
	return content, nil
}

var _ tool.Toolbox = (*QueueToolbox)(nil)
