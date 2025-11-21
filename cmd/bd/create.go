package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/fatih/color"
	"github.com/spf13/cobra"
	"github.com/steveyegge/beads/internal/beads"
	"github.com/steveyegge/beads/internal/config"
	"github.com/steveyegge/beads/internal/debug"
	"github.com/steveyegge/beads/internal/routing"
	"github.com/steveyegge/beads/internal/rpc"
	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/storage/sqlite"
	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/internal/validation"
)

var createCmd = &cobra.Command{
	Use:     "create [title]",
	Aliases: []string{"new"},
	Short:   "Create a new issue (or multiple issues from markdown file)",
	Args:    cobra.MinimumNArgs(0), // Changed to allow no args when using -f
	Run: func(cmd *cobra.Command, args []string) {
		file, _ := cmd.Flags().GetString("file")
		fromTemplate, _ := cmd.Flags().GetString("from-template")

		// If file flag is provided, parse markdown and create multiple issues
		if file != "" {
			if len(args) > 0 {
				fmt.Fprintf(os.Stderr, "Error: cannot specify both title and --file flag\n")
				os.Exit(1)
			}
			createIssuesFromMarkdown(cmd, file)
			return
		}

		// Original single-issue creation logic
		// Get title from flag or positional argument
		titleFlag, _ := cmd.Flags().GetString("title")
		var title string

		if len(args) > 0 && titleFlag != "" {
			// Both provided - check if they match
			if args[0] != titleFlag {
				fmt.Fprintf(os.Stderr, "Error: cannot specify different titles as both positional argument and --title flag\n")
				fmt.Fprintf(os.Stderr, "  Positional: %q\n", args[0])
				fmt.Fprintf(os.Stderr, "  --title:    %q\n", titleFlag)
				os.Exit(1)
			}
			title = args[0] // They're the same, use either
		} else if len(args) > 0 {
			title = args[0]
		} else if titleFlag != "" {
			title = titleFlag
		} else {
			fmt.Fprintf(os.Stderr, "Error: title required (or use --file to create from markdown)\n")
			os.Exit(1)
		}

		// Warn if creating a test issue in production database
		if strings.HasPrefix(strings.ToLower(title), "test") {
			yellow := color.New(color.FgYellow).SprintFunc()
			fmt.Fprintf(os.Stderr, "%s Creating issue with 'Test' prefix in production database.\n", yellow("⚠"))
			fmt.Fprintf(os.Stderr, "  For testing, consider using: BEADS_DB=/tmp/test.db ./bd create \"Test issue\"\n")
		}

		// Load template if specified
		var tmpl *Template
		if fromTemplate != "" {
			var err error
			tmpl, err = loadTemplate(fromTemplate)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
		}

		// Get field values, preferring explicit flags over template defaults
		description, _ := cmd.Flags().GetString("description")
		if description == "" && tmpl != nil {
			description = tmpl.Description
		}

		design, _ := cmd.Flags().GetString("design")
		if design == "" && tmpl != nil {
			design = tmpl.Design
		}

		acceptance, _ := cmd.Flags().GetString("acceptance")
		if acceptance == "" && tmpl != nil {
			acceptance = tmpl.AcceptanceCriteria
		}
		
		// Parse priority (supports both "1" and "P1" formats)
		priorityStr, _ := cmd.Flags().GetString("priority")
		priority, err := validation.ValidatePriority(priorityStr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		if cmd.Flags().Changed("priority") == false && tmpl != nil {
			priority = tmpl.Priority
		}

		issueType, _ := cmd.Flags().GetString("type")
		if !cmd.Flags().Changed("type") && tmpl != nil && tmpl.Type != "" {
			// Flag not explicitly set and template has a type, use template
			issueType = tmpl.Type
		}

		assignee, _ := cmd.Flags().GetString("assignee")

		labels, _ := cmd.Flags().GetStringSlice("labels")
		labelAlias, _ := cmd.Flags().GetStringSlice("label")
		if len(labelAlias) > 0 {
			labels = append(labels, labelAlias...)
		}
		if len(labels) == 0 && tmpl != nil && len(tmpl.Labels) > 0 {
			labels = tmpl.Labels
		}

		explicitID, _ := cmd.Flags().GetString("id")
		parentID, _ := cmd.Flags().GetString("parent")
		externalRef, _ := cmd.Flags().GetString("external-ref")
		deps, _ := cmd.Flags().GetStringSlice("deps")
		forceCreate, _ := cmd.Flags().GetBool("force")
		repoOverride, _ := cmd.Flags().GetString("repo")
		// Use global jsonOutput set by PersistentPreRun

		// Determine target repository using routing logic
		repoPath := "." // default to current directory
		if cmd.Flags().Changed("repo") {
			// Explicit --repo flag overrides auto-routing
			repoPath = repoOverride
		} else {
			// Auto-routing based on user role
			userRole, err := routing.DetectUserRole(".")
			if err != nil {
				debug.Logf("Warning: failed to detect user role: %v\n", err)
			}
			
			// Read routing config from database (bd config set) or viper (config file/env)
			// Priority: database config > viper config > defaults
			ctx := context.Background()
			getRoutingConfig := func(key, defaultVal string) string {
				// Try database config first (set via bd config set)
				if store != nil && daemonClient == nil {
					if val, err := store.GetConfig(ctx, key); err == nil && val != "" {
						return val
					}
				}
				// Fall back to viper config (config file or env vars)
				if val := config.GetString(key); val != "" {
					return val
				}
				return defaultVal
			}
			
			routingConfig := &routing.RoutingConfig{
				Mode:             getRoutingConfig("routing.mode", "auto"),
				DefaultRepo:      getRoutingConfig("routing.default", "."),
				MaintainerRepo:   getRoutingConfig("routing.maintainer", "."),
				ContributorRepo:  getRoutingConfig("routing.contributor", getRoutingConfig("contributor.planning_repo", "~/.beads-planning")),
				ExplicitOverride: repoOverride,
			}
			
			repoPath = routing.DetermineTargetRepo(routingConfig, userRole, ".")
		}
		
		// Switch to target repo for multi-repo support (bd-4ms, bd-3wo)
		var targetStore storage.Storage
		var targetStoreOwned bool // Track if we need to close the store
		var sourceRepo string // Track the source repo for the issue
		
		if repoPath != "." {
			// Route to a different repository - open its database
			targetDBPath := filepath.Join(repoPath, ".beads", beads.CanonicalDatabaseName)
			
			// Check if target database exists
			if _, err := os.Stat(targetDBPath); os.IsNotExist(err) {
				fmt.Fprintf(os.Stderr, "Error: target repository database not found at %s\n", targetDBPath)
				fmt.Fprintf(os.Stderr, "Hint: run 'bd init' in %s first\n", repoPath)
				os.Exit(1)
			}
			
			// Open target repository's database
			ctx := context.Background()
			targetStoreSQL, err := sqlite.New(ctx, targetDBPath)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error: failed to open target database at %s: %v\n", targetDBPath, err)
				os.Exit(1)
			}
			targetStore = targetStoreSQL
			targetStoreOwned = true
			defer func() {
				if targetStoreOwned && targetStore != nil {
					if sqliteStore, ok := targetStore.(*sqlite.SQLiteStorage); ok {
						_ = sqliteStore.Close()
					}
				}
			}()
			
			// Set source_repo to point back to current repo
			// This allows issues to reference their origin
			cwd, err := os.Getwd()
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error: failed to get current directory: %v\n", err)
				os.Exit(1)
			}
			sourceRepo = cwd
			
			debug.Logf("Routing to target repo: %s (source: %s)\n", repoPath, sourceRepo)
		} else {
			// Use current repository's store
			targetStore = store
			targetStoreOwned = false
			sourceRepo = "."
		}

		// Check for conflicting flags
		if explicitID != "" && parentID != "" {
			fmt.Fprintf(os.Stderr, "Error: cannot specify both --id and --parent flags\n")
			os.Exit(1)
		}

		// If parent is specified, generate child ID
		// In daemon mode, the parent will be sent to the RPC handler
		// In direct mode, we generate the child ID here
		if parentID != "" && daemonClient == nil {
			ctx := context.Background()
			childID, err := targetStore.GetNextChildID(ctx, parentID)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
			explicitID = childID // Set as explicit ID for the rest of the flow
		}

		// Validate explicit ID format if provided
		if explicitID != "" {
			requestedPrefix, err := validation.ValidateIDFormat(explicitID)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}

			// Validate prefix matches database prefix
			ctx := context.Background()

			// Get database prefix from config
			var dbPrefix string
			if daemonClient != nil {
				// Using daemon - need to get config via RPC
				// For now, skip validation in daemon mode (needs RPC enhancement)
			} else {
				// Direct mode - check config
				dbPrefix, _ = targetStore.GetConfig(ctx, "issue_prefix")
			}

			if !forceCreate && dbPrefix != "" && dbPrefix != requestedPrefix {
				fmt.Fprintf(os.Stderr, "Error: prefix mismatch detected\n")
				fmt.Fprintf(os.Stderr, "  This database uses prefix '%s', but you specified '%s'\n", dbPrefix, requestedPrefix)
				fmt.Fprintf(os.Stderr, "  Use --force to create with mismatched prefix anyway\n")
				os.Exit(1)
			}
		}

		var externalRefPtr *string
		if externalRef != "" {
			externalRefPtr = &externalRef
		}

		// If daemon is running, use RPC (but skip daemon if routing to different repo)
		if daemonClient != nil && repoPath == "." {
			createArgs := &rpc.CreateArgs{
				ID:                 explicitID,
				Parent:             parentID,
				Title:              title,
				Description:        description,
				IssueType:          issueType,
				Priority:           priority,
				Design:             design,
				AcceptanceCriteria: acceptance,
				Assignee:           assignee,
				ExternalRef:        externalRef,
				Labels:             labels,
				Dependencies:       deps,
			}

			resp, err := daemonClient.Create(createArgs)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}

			if jsonOutput {
				fmt.Println(string(resp.Data))
			} else {
				var issue types.Issue
				if err := json.Unmarshal(resp.Data, &issue); err != nil {
					fmt.Fprintf(os.Stderr, "Error parsing response: %v\n", err)
					os.Exit(1)
				}
				green := color.New(color.FgGreen).SprintFunc()
				fmt.Printf("%s Created issue: %s\n", green("✓"), issue.ID)
				fmt.Printf("  Title: %s\n", issue.Title)
				fmt.Printf("  Priority: P%d\n", issue.Priority)
				fmt.Printf("  Status: %s\n", issue.Status)
			}
			return
		}

		// Direct mode
		issue := &types.Issue{
			ID:                 explicitID, // Set explicit ID if provided (empty string if not)
			Title:              title,
			Description:        description,
			Design:             design,
			AcceptanceCriteria: acceptance,
			Status:             types.StatusOpen,
			Priority:           priority,
			IssueType:          types.IssueType(issueType),
			Assignee:           assignee,
			ExternalRef:        externalRefPtr,
		}

		ctx := rootCtx
		
		// Check if any dependencies are discovered-from type
		// If so, inherit source_repo from the parent issue
		var discoveredFromParentID string
		for _, depSpec := range deps {
			depSpec = strings.TrimSpace(depSpec)
			if depSpec == "" {
				continue
			}
			
			var depType types.DependencyType
			var dependsOnID string
			
			if strings.Contains(depSpec, ":") {
				parts := strings.SplitN(depSpec, ":", 2)
				if len(parts) == 2 {
					depType = types.DependencyType(strings.TrimSpace(parts[0]))
					dependsOnID = strings.TrimSpace(parts[1])
					
					if depType == types.DepDiscoveredFrom {
						discoveredFromParentID = dependsOnID
						break
					}
				}
			}
		}
		
		// If we found a discovered-from dependency, inherit source_repo from parent
		if discoveredFromParentID != "" {
			parentIssue, err := targetStore.GetIssue(ctx, discoveredFromParentID)
			if err == nil && parentIssue.SourceRepo != "" {
				issue.SourceRepo = parentIssue.SourceRepo
			}
			// If error getting parent or parent has no source_repo, continue with default
		}
		
		// Override source_repo if we routed to a different repository
		if sourceRepo != "." {
			issue.SourceRepo = sourceRepo
		}
		
		if err := targetStore.CreateIssue(ctx, issue, actor); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}

		// If parent was specified, add parent-child dependency
		if parentID != "" {
			dep := &types.Dependency{
				IssueID:     issue.ID,
				DependsOnID: parentID,
				Type:        types.DepParentChild,
			}
			if err := targetStore.AddDependency(ctx, dep, actor); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to add parent-child dependency %s -> %s: %v\n", issue.ID, parentID, err)
			}
		}

		// Add labels if specified
		for _, label := range labels {
			if err := targetStore.AddLabel(ctx, issue.ID, label, actor); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to add label %s: %v\n", label, err)
			}
		}

		// Add dependencies if specified (format: type:id or just id for default "blocks" type)
		for _, depSpec := range deps {
			// Skip empty specs (e.g., from trailing commas)
			depSpec = strings.TrimSpace(depSpec)
			if depSpec == "" {
				continue
			}

			var depType types.DependencyType
			var dependsOnID string

			// Parse format: "type:id" or just "id" (defaults to "blocks")
			if strings.Contains(depSpec, ":") {
				parts := strings.SplitN(depSpec, ":", 2)
				if len(parts) != 2 {
					fmt.Fprintf(os.Stderr, "Warning: invalid dependency format '%s', expected 'type:id' or 'id'\n", depSpec)
					continue
				}
				depType = types.DependencyType(strings.TrimSpace(parts[0]))
				dependsOnID = strings.TrimSpace(parts[1])
			} else {
				// Default to "blocks" if no type specified
				depType = types.DepBlocks
				dependsOnID = depSpec
			}

			// Validate dependency type
			if !depType.IsValid() {
				fmt.Fprintf(os.Stderr, "Warning: invalid dependency type '%s' (valid: blocks, related, parent-child, discovered-from)\n", depType)
				continue
			}

			// Add the dependency
			dep := &types.Dependency{
				IssueID:     issue.ID,
				DependsOnID: dependsOnID,
				Type:        depType,
			}
			if err := targetStore.AddDependency(ctx, dep, actor); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to add dependency %s -> %s: %v\n", issue.ID, dependsOnID, err)
			}
		}

		// Schedule auto-flush
		markDirtyAndScheduleFlush()

		if jsonOutput {
			outputJSON(issue)
		} else {
			green := color.New(color.FgGreen).SprintFunc()
			fmt.Printf("%s Created issue: %s\n", green("✓"), issue.ID)
			fmt.Printf("  Title: %s\n", issue.Title)
			fmt.Printf("  Priority: P%d\n", issue.Priority)
			fmt.Printf("  Status: %s\n", issue.Status)
		}
	},
}

func init() {
	createCmd.Flags().StringP("file", "f", "", "Create multiple issues from markdown file")
	createCmd.Flags().String("from-template", "", "Create issue from template (e.g., 'epic', 'bug', 'feature')")
	createCmd.Flags().String("title", "", "Issue title (alternative to positional argument)")
	registerPriorityFlag(createCmd, "2")
	createCmd.Flags().StringP("type", "t", "task", "Issue type (bug|feature|task|epic|chore)")
	registerCommonIssueFlags(createCmd)
	createCmd.Flags().StringSliceP("labels", "l", []string{}, "Labels (comma-separated)")
	createCmd.Flags().StringSlice("label", []string{}, "Alias for --labels")
	_ = createCmd.Flags().MarkHidden("label")
	createCmd.Flags().String("id", "", "Explicit issue ID (e.g., 'bd-42' for partitioning)")
	createCmd.Flags().String("parent", "", "Parent issue ID for hierarchical child (e.g., 'bd-a3f8e9')")
	createCmd.Flags().StringSlice("deps", []string{}, "Dependencies in format 'type:id' or 'id' (e.g., 'discovered-from:bd-20,blocks:bd-15' or 'bd-20')")
	createCmd.Flags().Bool("force", false, "Force creation even if prefix doesn't match database prefix")
	createCmd.Flags().String("repo", "", "Target repository for issue (overrides auto-routing)")
	// Note: --json flag is defined as a persistent flag in main.go, not here
	rootCmd.AddCommand(createCmd)
}
