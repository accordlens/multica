package main

import (
	"context"
	"fmt"
	"os"

	"github.com/multica-ai/multica/server/internal/cli"
	"github.com/spf13/cobra"
)

func newIssueLinkCmd() *cobra.Command {
	root := &cobra.Command{Use: "link", Short: "Inspect and report exact run, PR, commit and artifact provenance"}
	for _, spec := range []struct {
		use, short string
		args       int
	}{
		{"list <issue-id>", "List native run evidence and recorded source links", 1},
		{"add <issue-id>", "Report an exact PR, commit or artifact for a known run", 1},
		{"remove <issue-id> <link-id>", "Remove a provenance pointer, retaining its history", 2},
	} {
		cmd := &cobra.Command{Use: spec.use, Short: spec.short, Args: exactArgs(spec.args), RunE: runIssueLink}
		cmd.Flags().String("output", "table", "Output format: table or json")
		if cmd.Name() == "list" {
			cmd.Flags().Bool("history", false, "Include removed pointers")
			cmd.Flags().Int("offset", 0, "Pagination offset for runs and artifacts (100 per page)")
		} else {
			cmd.Flags().String("run-id", "", "Native source run UUID (required)")
		}
		if cmd.Name() == "add" {
			cmd.Flags().String("kind", "", "commit, pull_request or artifact")
			cmd.Flags().String("url", "", "Exact HTTPS pull request URL")
			cmd.Flags().String("commit-sha", "", "Full commit SHA")
			cmd.Flags().String("artifact-id", "", "Artifact UUID belonging to this issue/run")
			cmd.Flags().String("sha256", "", "Artifact/report SHA256")
		}
		root.AddCommand(cmd)
	}
	return root
}

func init() { issueCmd.AddCommand(newIssueLinkCmd()) }

func runIssueLink(cmd *cobra.Command, args []string) error {
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	issue, err := resolveIssueRef(ctx, client, args[0])
	if err != nil {
		return fmt.Errorf("resolve issue: %w", err)
	}
	path := "/api/issues/" + issue.ID + "/links"
	var result map[string]any
	if cmd.Name() == "list" {
		offset, _ := cmd.Flags().GetInt("offset")
		if offset < 0 || offset > 1000000 {
			return fmt.Errorf("invalid offset")
		}
		history, _ := cmd.Flags().GetBool("history")
		err = client.GetJSON(ctx, fmt.Sprintf("%s?offset=%d&include_removed=%t", path, offset, history), &result)
	} else {
		run, _ := cmd.Flags().GetString("run-id")
		if run == "" {
			return fmt.Errorf("--run-id is required")
		}
		body := map[string]string{"run_id": run}
		if cmd.Name() == "add" {
			for _, pair := range [][2]string{{"kind", "kind"}, {"url", "url"}, {"commit-sha", "commit_sha"}, {"artifact-id", "artifact_id"}, {"sha256", "sha256"}} {
				value, _ := cmd.Flags().GetString(pair[0])
				if value != "" {
					body[pair[1]] = value
				}
			}
		} else {
			path += "/" + args[1] + "/remove"
		}
		err = client.PostJSON(ctx, path, body, &result)
	}
	if err != nil {
		return fmt.Errorf("%s provenance link: %w", cmd.Name(), err)
	}
	output, _ := cmd.Flags().GetString("output")
	if output == "json" {
		return cli.PrintJSON(os.Stdout, result)
	}
	if cmd.Name() != "list" {
		fmt.Fprintln(os.Stdout, "Provenance link updated.")
		return nil
	}
	rows := [][]string{}
	items, _ := result["links"].([]any)
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		ref := strVal(item, "url")
		if ref == "" {
			ref = strVal(item, "commit_sha")
		}
		if ref == "" {
			ref = strVal(item, "artifact_id")
		}
		rows = append(rows, []string{strVal(item, "id"), strVal(item, "kind"), strVal(item, "run_id"), strVal(item, "step"), ref})
	}
	cli.PrintTable(os.Stdout, []string{"LINK ID", "KIND", "RUN", "STEP", "REFERENCE"}, rows)
	if more, _ := result["has_more"].(bool); more {
		fmt.Fprintln(os.Stdout, "More results: increase --offset by 100.")
	}
	fmt.Fprintln(os.Stdout, "Use --output json for native run evidence, revision history and artifacts.")
	return nil
}
