package main

import (
	"context"
	"fmt"
	"os"

	"github.com/multica-ai/multica/server/internal/cli"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
	"github.com/spf13/cobra"
)

func newIssueRelationCmd() *cobra.Command {
	root := &cobra.Command{Use: "relation", Short: "Manage execution dependencies and context links"}
	for _, spec := range []struct {
		use, short string
		args       int
	}{
		{"list <issue-id>", "List relations, readiness and waiting reasons", 1},
		{"add <issue-id> <other-issue-id>", "Add an explicit dependency or context link", 2},
		{"remove <issue-id> <relation-id>", "Remove a relation (retains its audit history)", 2},
		{"accept <issue-id> <relation-id>", "Accept a delivered source revision (human credentials required)", 2},
	} {
		cmd := &cobra.Command{Use: spec.use, Short: spec.short, Args: exactArgs(spec.args), RunE: runIssueRelation}
		cmd.Flags().String("output", "table", "Output format: table or json")
		root.AddCommand(cmd)
		switch cmd.Name() {
		case "list":
			cmd.Flags().Bool("history", false, "Include removed relations")
		case "add":
			cmd.Flags().String("type", "blocked_by", "blocked_by, blocks or related")
			cmd.Flags().String("condition", "done", "done, in_review, accepted or run_completed")
			cmd.Flags().String("step", "", "Dependent workflow status key (empty applies to every step)")
			cmd.Flags().String("run-id", "", "Exact source run UUID for run_completed")
			cmd.Flags().String("source-step", "", "Source delivery/entry workflow status key")
		case "accept":
			cmd.Flags().String("evidence-version", "", "Source evidence version displayed by relation list (required)")
			cmd.Flags().Int64("revision", 0, "Exact source issue revision displayed by relation list (required)")
		}
	}
	return root
}

func init() { issueCmd.AddCommand(newIssueRelationCmd()) }

func runIssueRelation(cmd *cobra.Command, args []string) error {
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
	path := "/api/issues/" + issue.ID + "/relations"
	var result map[string]any
	switch cmd.Name() {
	case "list":
		history, _ := cmd.Flags().GetBool("history")
		if history {
			path += "?include_removed=true"
		}
		err = client.GetJSON(ctx, path, &result)
	case "add":
		other, resolveErr := resolveIssueRef(ctx, client, args[1])
		if resolveErr != nil {
			return fmt.Errorf("resolve other issue: %w", resolveErr)
		}
		typ, _ := cmd.Flags().GetString("type")
		kind, _ := cmd.Flags().GetString("condition")
		run, _ := cmd.Flags().GetString("run-id")
		sourceStep, _ := cmd.Flags().GetString("source-step")
		step, _ := cmd.Flags().GetString("step")
		condition := issueworkflow.DependencyCondition{Kind: kind, RunID: run, SourceStep: sourceStep}
		if err := condition.Validate(); err != nil {
			return err
		}
		err = client.PostJSON(ctx, path, map[string]any{"issue_id": other.ID, "type": typ, "condition": condition, "target_step": step}, &result)
	case "remove":
		err = client.DeleteJSON(ctx, path+"/"+args[1])
		result = map[string]any{"removed": true}
	case "accept":
		revision, _ := cmd.Flags().GetInt64("revision")
		if !cmd.Flags().Changed("revision") {
			return fmt.Errorf("--revision is required; inspect the source revision with relation list")
		}
		version, _ := cmd.Flags().GetString("evidence-version")
		if version == "" {
			return fmt.Errorf("--evidence-version is required; inspect relation list")
		}
		err = client.PostJSON(ctx, path+"/"+args[1]+"/accept", map[string]any{"revision": revision, "evidence_version": version}, &result)
	}
	if err != nil {
		return fmt.Errorf("%s relation: %w", cmd.Name(), err)
	}
	output, _ := cmd.Flags().GetString("output")
	if output == "json" {
		return cli.PrintJSON(os.Stdout, result)
	}
	if cmd.Name() != "list" {
		fmt.Fprintln(os.Stdout, "Relation updated.")
		return nil
	}
	fmt.Fprintf(os.Stdout, "Step: %v; ready: %v\n", result["step"], result["ready"])
	rows := [][]string{}
	items, _ := result["relations"].([]any)
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		other, _ := item["issue"].(map[string]any)
		condition, _ := item["condition"].(map[string]any)
		rows = append(rows, []string{strVal(item, "id"), strVal(item, "type"), strVal(other, "identifier"), strVal(condition, "kind"), strVal(item, "reason"), fmt.Sprint(item["source_revision"]), strVal(item, "source_evidence_version")})
	}
	cli.PrintTable(os.Stdout, []string{"ID", "RELATION", "ISSUE", "CONDITION", "WAITING", "SOURCE REVISION", "EVIDENCE VERSION"}, rows)
	return nil
}
