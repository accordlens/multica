package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"github.com/multica-ai/multica/server/internal/issueworkflow"
)

// The helper outlives its parent's address space, but only for cleanup. EOF on
// the lease pipe cancels the entire owned command tree after a daemon crash.
const WorkflowScriptHelperArg = "__multica_workflow_script"

type workflowScriptRequest struct {
	RunID     string                  `json:"run_id"`
	Execution issueworkflow.Execution `json:"execution"`
}
type workflowScriptResponse struct {
	Evidence  issueworkflow.Evidence `json:"evidence"`
	Error     string                 `json:"error,omitempty"`
	Duplicate bool                   `json:"duplicate,omitempty"`
}

func RunWorkflowScriptHelper(input io.Reader, output io.Writer) error {
	decoder := json.NewDecoder(input)
	var request workflowScriptRequest
	if err := decoder.Decode(&request); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _, _ = io.Copy(io.Discard, input); cancel() }()
	evidence, err := runWorkflowScript(ctx, request.RunID, request.Execution)
	response := workflowScriptResponse{Evidence: evidence, Duplicate: errors.Is(err, errStartClaimRejected)}
	if err != nil {
		response.Error = err.Error()
	}
	return json.NewEncoder(output).Encode(response)
}

func superviseWorkflowScript(ctx context.Context, runID string, execution issueworkflow.Execution) (issueworkflow.Evidence, error) {
	executable, err := os.Executable()
	if err != nil {
		return issueworkflow.Evidence{}, err
	}
	cmd := exec.Command(executable, WorkflowScriptHelperArg)
	cmd.Env = workflowEnvironment("", "", "", "", "")
	return superviseWorkflowCommand(ctx, cmd, runID, execution)
}

func superviseWorkflowCommand(ctx context.Context, cmd *exec.Cmd, runID string, execution issueworkflow.Execution) (issueworkflow.Evidence, error) {
	configureWorkflowSupervisor(cmd)
	lease, err := cmd.StdinPipe()
	if err != nil {
		return issueworkflow.Evidence{}, err
	}
	defer lease.Close()
	var output limitedWorkflowOutput
	cmd.Stdout = &output
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return issueworkflow.Evidence{}, err
	}
	requestErr := json.NewEncoder(lease).Encode(workflowScriptRequest{RunID: runID, Execution: execution})
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	if requestErr != nil {
		_ = lease.Close()
	}
	select {
	case err = <-done:
	case <-ctx.Done():
		_ = lease.Close() // The helper cleans up and retains its receipt.
		select {
		case err = <-done:
		case <-time.After(12 * time.Second):
			// The exact helper PID belongs to this invocation; no daemon is
			// discovered or terminated by executable name.
			_ = cmd.Process.Kill()
			err = <-done
			return issueworkflow.Evidence{}, fmt.Errorf("workflow helper cleanup could not be confirmed: %w", err)
		}
	}
	if err != nil {
		return issueworkflow.Evidence{}, err
	}
	var response workflowScriptResponse
	if output.overflow || json.Unmarshal(output.data, &response) != nil {
		return issueworkflow.Evidence{}, fmt.Errorf("invalid workflow helper response")
	}
	if response.Duplicate {
		return response.Evidence, errStartClaimRejected
	}
	// The error is already a typed outcome in the receipt, not an excuse to
	// replay a potentially partially-executed command.
	return response.Evidence, nil
}

type limitedWorkflowOutput struct {
	data     []byte
	overflow bool
}

func (b *limitedWorkflowOutput) Write(p []byte) (int, error) {
	if len(b.data)+len(p) > 1024*1024 {
		b.overflow = true
	} else {
		b.data = append(b.data, p...)
	}
	return len(p), nil
}
