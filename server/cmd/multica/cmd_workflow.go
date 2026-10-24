package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
)

var workflowCmd = &cobra.Command{Use: "workflow", Short: "Read and configure project workflows"}

func init() {
	workflowCmd.PersistentFlags().String("output", "json", "Output format (json)")
	list := &cobra.Command{Use: "list", Short: "List workspace workflows", Args: exactArgs(0), RunE: runWorkflowRead}
	get := &cobra.Command{Use: "get <workflow-id>", Short: "Read a workflow and its registered script gates", Args: exactArgs(1), RunE: runWorkflowRead}
	create := &cobra.Command{Use: "create", Short: "Create a reviewed workflow from a JSON definition", Args: exactArgs(0), RunE: runWorkflowWrite}
	update := &cobra.Command{Use: "update <workflow-id>", Short: "Update a workflow from a reviewed JSON definition", Args: exactArgs(1), RunE: runWorkflowWrite}
	use := &cobra.Command{Use: "use <project-id> <workflow-id>", Short: "Bind a workflow to a project", Args: exactArgs(2), RunE: runWorkflowUse}
	for _, cmd := range []*cobra.Command{create, update} {
		cmd.Flags().String("definition", "", "Reviewed workflow JSON file")
		_ = cmd.MarkFlagRequired("definition")
	}
	for _, cmd := range []*cobra.Command{list, get, create, update, use} {
		workflowCmd.AddCommand(cmd)
	}
	workflowCmd.GroupID = groupCore
	rootCmd.AddCommand(workflowCmd)
}

func runWorkflowRead(cmd *cobra.Command, args []string) error {
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	path := "/api/issue-workflows"
	if len(args) > 0 {
		path += "/" + url.PathEscape(args[0])
	}
	var result any
	if err := client.GetJSON(ctx, path, &result); err != nil {
		return err
	}
	return cli.PrintJSON(os.Stdout, result)
}

func workflowDefinition(filename string) (map[string]any, error) {
	raw, err := os.ReadFile(filename)
	if err != nil {
		return nil, err
	}
	if len(raw) > 1024*1024 {
		return nil, fmt.Errorf("workflow definition exceeds 1 MiB")
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, fmt.Errorf("invalid workflow JSON: %w", err)
	}
	if body == nil {
		return nil, fmt.Errorf("workflow definition must be a JSON object")
	}
	return body, nil
}

func runWorkflowWrite(cmd *cobra.Command, args []string) error {
	filename, _ := cmd.Flags().GetString("definition")
	body, err := workflowDefinition(filename)
	if err != nil {
		return err
	}
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	var result any
	if len(args) == 0 {
		err = client.PostJSON(ctx, "/api/issue-workflows", body, &result)
	} else {
		err = client.PatchJSON(ctx, "/api/issue-workflows/"+url.PathEscape(args[0]), body, &result)
	}
	if err != nil {
		return err
	}
	return cli.PrintJSON(os.Stdout, result)
}

func runWorkflowUse(cmd *cobra.Command, args []string) error {
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	var result any
	if err := client.PostJSON(ctx, "/api/projects/"+url.PathEscape(args[0])+"/workflow", map[string]any{"workflow_id": args[1]}, &result); err != nil {
		return err
	}
	return cli.PrintJSON(os.Stdout, result)
}
