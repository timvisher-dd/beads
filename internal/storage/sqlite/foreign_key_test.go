package sqlite

import (
	"context"
	"testing"

	"github.com/steveyegge/beads/internal/types"
)

// TestUpdateIssueIDPreservesForeignKeys tests that UpdateIssueID properly
// re-enables foreign keys after temporarily disabling them, preventing
// foreign key constraint failures in subsequent operations (bd-bib).
func TestUpdateIssueIDPreservesForeignKeys(t *testing.T) {
	ctx := context.Background()
	store, err := New(ctx, ":memory:")
	if err != nil {
		t.Fatalf("Failed to create storage: %v", err)
	}
	defer func() { _ = store.Close() }()

	// Initialize database with prefix
	if err := store.SetConfig(ctx, "issue_prefix", "test"); err != nil {
		t.Fatalf("Failed to set issue prefix: %v", err)
	}

	// Create an issue
	issue := &types.Issue{
		ID:          "test-abc",
		Title:       "Test Issue",
		Description: "Test",
		Status:      types.StatusOpen,
		Priority:    2,
		IssueType:   types.TypeTask,
	}
	if err := store.CreateIssue(ctx, issue, "test-actor"); err != nil {
		t.Fatalf("Failed to create issue: %v", err)
	}

	// Add a comment (requires foreign key constraint)
	_, err = store.AddIssueComment(ctx, "test-abc", "test-author", "Test comment")
	if err != nil {
		t.Fatalf("Failed to add comment before rename: %v", err)
	}

	// Rename the issue (this temporarily disables foreign keys)
	renamedIssue := &types.Issue{
		ID:          "test-xyz",
		Title:       "Test Issue",
		Description: "Test",
		Status:      types.StatusOpen,
		Priority:    2,
		IssueType:   types.TypeTask,
	}
	if err := store.UpdateIssueID(ctx, "test-abc", "test-xyz", renamedIssue, "test-actor"); err != nil {
		t.Fatalf("Failed to rename issue: %v", err)
	}

	// Try to close the issue (requires foreign keys to be enabled for event insertion)
	// This is where bd-bib was failing with "FOREIGN KEY constraint failed"
	if err := store.CloseIssue(ctx, "test-xyz", "Testing", "test-actor"); err != nil {
		t.Fatalf("Failed to close issue after rename: %v (foreign keys not properly re-enabled)", err)
	}

	// Verify the issue is actually closed
	closedIssue, err := store.GetIssue(ctx, "test-xyz")
	if err != nil {
		t.Fatalf("Failed to get renamed issue: %v", err)
	}
	if closedIssue.Status != types.StatusClosed {
		t.Errorf("Issue status = %v, want %v", closedIssue.Status, types.StatusClosed)
	}

	// Verify we can add another comment (foreign key constraint check)
	_, err = store.AddIssueComment(ctx, "test-xyz", "test-author", "Another comment")
	if err != nil {
		t.Fatalf("Failed to add comment after close: %v (foreign keys not working)", err)
	}
}
