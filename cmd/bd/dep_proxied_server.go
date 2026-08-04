package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/steveyegge/beads/internal/storage/domain"
	"github.com/steveyegge/beads/internal/storage/uow"
	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/internal/ui"
	"github.com/steveyegge/beads/issueops"
)

type depAddResult struct {
	fromTitle string
	toTitle   string
	cycles    [][]*types.Issue
	cycleErr  error
}

// proxiedDependencyEditor hands back the guarded dependency-edge surface for
// the proxied-server provider, through the provider's OWN capability accessor
// — the same two-step proxiedIssueReader and proxiedBatchCloser perform, and
// for the same reason: the accessor is where each layer is added, so a command
// that reached for the constructor would get an unlayered editor.
func proxiedDependencyEditor() (issueops.DependencyEditor, error) {
	if uowProvider == nil {
		return nil, errors.New("proxied-server UOW provider not initialized")
	}
	src, ok := uowProvider.(uow.DependencyEditorSource)
	if !ok {
		return nil, fmt.Errorf("proxied-server provider %T does not offer the dependency-edge surface", uowProvider)
	}
	return src.DependencyEditor()
}

// proxiedIssueRelations hands back the guarded neighbor-query surface for the
// proxied-server provider, through the provider's own capability accessor.
func proxiedIssueRelations() (issueops.Relations, error) {
	if uowProvider == nil {
		return nil, errors.New("proxied-server UOW provider not initialized")
	}
	src, ok := uowProvider.(uow.RelationsSource)
	if !ok {
		return nil, fmt.Errorf("proxied-server provider %T does not offer the neighbor-query surface", uowProvider)
	}
	return src.IssueRelations()
}

// addDependencyEdgesProxied asserts edges through the DependencyEditor role.
//
// skipPerEdgeCycleCheck is a separate argument from the --no-cycle-check flag
// on purpose. That flag has never turned the per-edge probe off for a single
// edge on either route — it turns off the whole-graph sweep this command
// prints warnings from. Only the bulk path trades the per-edge probe away, and
// only because it always has.
func addDependencyEdgesProxied(ctx context.Context, edges []issueops.DependencyEdge, skipPerEdgeCycleCheck bool) error {
	editor, err := proxiedDependencyEditor()
	if err != nil {
		return err
	}
	_, err = editor.AddDependencies(ctx, issueops.AddDependenciesRequest{
		Actor:                 actor,
		Edges:                 edges,
		SkipPerEdgeCycleCheck: skipPerEdgeCycleCheck,
	})
	return err
}

// depEdgeFeedback gathers the cycle sweep and the titles the confirmation line
// wants, in a second READ-ONLY unit of work once the edges have landed.
//
// Neither belongs in the write. The role's request IS the transaction, and a
// cycle warning computed inside a transaction that has not committed describes
// a graph nobody else can see; a title is presentation. Failing to open the
// unit of work cannot fail the command either — the edges are already durable
// — so it is reported the way a failed sweep already was.
func depEdgeFeedback(ctx context.Context, fromID, toID string, checkCycles bool) depAddResult {
	var res depAddResult
	if fromID == "" && toID == "" && !checkCycles {
		return res
	}
	if uowProvider == nil {
		res.cycleErr = errors.New("proxied-server UOW provider not initialized")
		return res
	}
	uw, err := uowProvider.NewUOW(ctx)
	if err != nil {
		res.cycleErr = fmt.Errorf("open unit of work: %w", err)
		return res
	}
	defer uw.Close(ctx)

	if fromID != "" {
		res.fromTitle = proxiedLookupTitle(ctx, uw, fromID)
	}
	if toID != "" {
		res.toTitle = proxiedLookupTitle(ctx, uw, toID)
	}
	if checkCycles {
		res.cycles, res.cycleErr = uw.DependencyUseCase().DetectCycles(ctx)
	}
	return res
}

func proxiedLookupTitle(ctx context.Context, uw uow.UnitOfWork, id string) string {
	if IsExternalRef(id) {
		return ""
	}
	issue, err := uw.IssueUseCase().GetIssue(ctx, id)
	if err == nil && issue != nil {
		return issue.Title
	}
	wisp, err := uw.IssueUseCase().GetWisp(ctx, id)
	if err == nil && wisp != nil {
		return wisp.Title
	}
	return ""
}

func printCycleDetectionError(err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: Failed to check for cycles: %v\n", err)
	}
}

func printCycleWarnings(cycles [][]*types.Issue) {
	if len(cycles) == 0 {
		return
	}
	fmt.Fprintf(os.Stderr, "\n%s Warning: Dependency cycle detected!\n", ui.RenderWarn("⚠"))
	fmt.Fprintf(os.Stderr, "This can hide issues from the ready work list and cause confusion.\n\n")
	fmt.Fprintf(os.Stderr, "Cycle path:\n")
	for _, cycle := range cycles {
		for j, issue := range cycle {
			if j == 0 {
				fmt.Fprintf(os.Stderr, "  %s", issue.ID)
			} else {
				fmt.Fprintf(os.Stderr, " → %s", issue.ID)
			}
		}
		if len(cycle) > 0 {
			fmt.Fprintf(os.Stderr, " → %s", cycle[0].ID)
		}
		fmt.Fprintf(os.Stderr, "\n")
	}
	fmt.Fprintf(os.Stderr, "\nRun 'bd dep cycles' for detailed analysis.\n\n")
}

func runDepBlocksProxiedServer(cmd *cobra.Command, ctx context.Context, blockerID, blockedID string) error {
	if isDisallowedHierarchicalDependency(blockedID, blockerID, types.DepBlocks) {
		return HandleErrorRespectJSON("cannot add dependency: %s is already a child of %s. Children inherit dependency on parent completion via hierarchy. Adding an explicit dependency would create a deadlock", blockedID, blockerID)
	}

	noCycleCheck, _ := cmd.Flags().GetBool("no-cycle-check")

	edge := issueops.DependencyEdge{IssueID: blockedID, DependsOnID: blockerID, Type: types.DepBlocks}
	if err := addDependencyEdgesProxied(ctx, []issueops.DependencyEdge{edge}, false); err != nil {
		return HandleErrorRespectJSON("%v", err)
	}
	res := depEdgeFeedback(ctx, blockedID, blockerID, !noCycleCheck)

	printCycleDetectionError(res.cycleErr)
	printCycleWarnings(res.cycles)

	if jsonOutput {
		_ = outputJSON(map[string]interface{}{
			"status":     "added",
			"blocker_id": blockerID,
			"blocked_id": blockedID,
			"type":       string(types.DepBlocks),
		})
		return nil
	}

	fmt.Printf("%s Added dependency: %s blocks %s\n",
		ui.RenderPass("✓"),
		formatFeedbackIDParen(blockerID, res.toTitle),
		formatFeedbackIDParen(blockedID, res.fromTitle))
	return nil
}

func runDepAddProxiedServer(cmd *cobra.Command, ctx context.Context, args []string) error {
	depType, _ := cmd.Flags().GetString("type")
	file, _ := cmd.Flags().GetString("file")

	if file != "" {
		return runDepAddBulkProxied(cmd, ctx, file, depType)
	}

	blockedBy, _ := cmd.Flags().GetString("blocked-by")
	dependsOn, _ := cmd.Flags().GetString("depends-on")

	var dependsOnArg string
	switch {
	case blockedBy != "":
		dependsOnArg = blockedBy
	case dependsOn != "":
		dependsOnArg = dependsOn
	default:
		dependsOnArg = args[1]
	}

	fromID := args[0]
	var toID string
	if strings.HasPrefix(dependsOnArg, "external:") {
		if err := validateExternalRef(dependsOnArg); err != nil {
			return HandleErrorRespectJSON("%v", err)
		}
		toID = dependsOnArg
	} else {
		toID = dependsOnArg
	}

	dt := canonicalDependencyType(types.DependencyType(depType))
	if isDisallowedHierarchicalDependency(fromID, toID, dt) {
		return HandleErrorRespectJSON("cannot add dependency: %s is already a child of %s. Children inherit dependency on parent completion via hierarchy. Adding an explicit dependency would create a deadlock", fromID, toID)
	}

	if err := validateDependencyType(dt); err != nil {
		return HandleErrorRespectJSON("%v", err)
	}

	noCycleCheck, _ := cmd.Flags().GetBool("no-cycle-check")

	edge := issueops.DependencyEdge{IssueID: fromID, DependsOnID: toID, Type: dt}
	if err := addDependencyEdgesProxied(ctx, []issueops.DependencyEdge{edge}, false); err != nil {
		return HandleErrorRespectJSON("%v", err)
	}
	res := depEdgeFeedback(ctx, fromID, toID, !noCycleCheck)

	printCycleDetectionError(res.cycleErr)
	printCycleWarnings(res.cycles)

	if jsonOutput {
		_ = outputJSON(map[string]interface{}{
			"status":        "added",
			"issue_id":      fromID,
			"depends_on_id": toID,
			"type":          string(dt),
		})
		return nil
	}

	fmt.Printf("%s Added dependency: %s depends on %s (%s)\n",
		ui.RenderPass("✓"),
		formatFeedbackIDParen(fromID, res.fromTitle),
		formatFeedbackIDParen(toID, res.toTitle),
		dt)
	return nil
}

func runDepAddBulkProxied(cmd *cobra.Command, ctx context.Context, file, defaultType string) error {
	edges, err := readBulkDepEdges(file, defaultType)
	if err != nil {
		return HandleErrorRespectJSON("%v", err)
	}
	if len(edges) == 0 {
		return HandleErrorRespectJSON("no dependency edges found")
	}

	depEdges := make([]issueops.DependencyEdge, 0, len(edges))
	for _, edge := range edges {
		if isDisallowedHierarchicalDependency(edge.IssueID, edge.DependsOnID, edge.Type) {
			return HandleErrorRespectJSON("line %d: cannot add dependency: %s is already a child of %s", edge.Line, edge.IssueID, edge.DependsOnID)
		}
		if strings.HasPrefix(edge.DependsOnID, "external:") {
			if err := validateExternalRef(edge.DependsOnID); err != nil {
				return HandleErrorRespectJSON("line %d: %v", edge.Line, err)
			}
		}
		depEdges = append(depEdges, issueops.DependencyEdge{
			IssueID:     edge.IssueID,
			DependsOnID: edge.DependsOnID,
			Type:        edge.Type,
		})
	}

	noCycleCheck, _ := cmd.Flags().GetBool("no-cycle-check")

	if err := addDependencyEdgesProxied(ctx, depEdges, noCycleCheck); err != nil {
		return HandleErrorRespectJSON("%v", err)
	}
	res := depEdgeFeedback(ctx, "", "", !noCycleCheck)

	printCycleDetectionError(res.cycleErr)
	printCycleWarnings(res.cycles)

	if jsonOutput {
		out := make([]map[string]interface{}, 0, len(depEdges))
		for _, edge := range depEdges {
			out = append(out, map[string]interface{}{
				"issue_id":      edge.IssueID,
				"depends_on_id": edge.DependsOnID,
				"type":          string(edge.Type),
			})
		}
		_ = outputJSON(map[string]interface{}{
			"status":       "added",
			"count":        len(depEdges),
			"dependencies": out,
		})
		return nil
	}

	fmt.Printf("%s Added %d dependencies\n", ui.RenderPass("✓"), len(depEdges))
	return nil
}

func runDepRemoveProxiedServer(_ *cobra.Command, ctx context.Context, args []string) error {
	fromID := args[0]
	toID := args[1]
	if strings.HasPrefix(toID, "external:") {
		if err := validateExternalRef(toID); err != nil {
			return HandleErrorRespectJSON("%v", err)
		}
	}

	editor, err := proxiedDependencyEditor()
	if err != nil {
		return HandleErrorRespectJSON("%v", err)
	}
	// The role's Removed verdict is not printed. `bd dep remove` has always
	// confirmed the same way whether or not an edge was there, and reporting
	// the difference now would change what every existing script reads.
	if _, err := editor.RemoveDependency(ctx, issueops.RemoveDependencyRequest{
		Actor:       actor,
		IssueID:     fromID,
		DependsOnID: toID,
	}); err != nil {
		return HandleErrorRespectJSON("%v", err)
	}
	res := depEdgeFeedback(ctx, fromID, toID, false)

	if jsonOutput {
		_ = outputJSON(map[string]interface{}{
			"status":        "removed",
			"issue_id":      fromID,
			"depends_on_id": toID,
		})
		return nil
	}

	fmt.Printf("%s Removed dependency: %s no longer depends on %s\n",
		ui.RenderPass("✓"),
		formatFeedbackIDParen(fromID, res.fromTitle),
		formatFeedbackIDParen(toID, res.toTitle))
	return nil
}

func runDepListProxiedServer(cmd *cobra.Command, ctx context.Context, args []string) error {
	direction, _ := cmd.Flags().GetString("direction")
	typeFilter, _ := cmd.Flags().GetString("type")
	if direction == "" {
		direction = "down"
	}

	// The multi-id edge listing is a different question with a different
	// answer shape — raw edge records keyed by source, printed per source —
	// and no role describes it. It keeps its own unit of work.
	if len(args) > 1 && direction == "down" {
		return runDepListRecordsProxiedServer(ctx, args, typeFilter)
	}

	// Everything else is the neighbor query, and it is on the Relations role:
	// one call per anchor, each with an explicit direction, because the role
	// refuses to guess one.
	rel, err := proxiedIssueRelations()
	if err != nil {
		return HandleErrorRespectJSON("%v", err)
	}
	request := issueops.RelatedRequest{Direction: issueops.RelationOut}
	if direction == "up" {
		request.Direction = issueops.RelationIn
	}
	if typeFilter != "" {
		request.Types = []types.DependencyType{types.DependencyType(typeFilter)}
	}

	var allIssues []*issueops.RelatedIssue
	for _, id := range args {
		request.ID = id
		issues, err := rel.Related(ctx, request)
		if err != nil {
			return HandleErrorRespectJSON("%v", err)
		}
		allIssues = append(allIssues, issues...)
	}

	if jsonOutput {
		if allIssues == nil {
			allIssues = []*issueops.RelatedIssue{}
		}
		_ = outputJSON(allIssues)
		return nil
	}

	if len(allIssues) == 0 {
		if len(args) == 1 {
			if direction == "up" {
				fmt.Printf("\nNo issues depend on %s\n", args[0])
			} else {
				fmt.Printf("\n%s has no dependencies\n", args[0])
			}
		} else {
			fmt.Println("\nNo dependencies found")
		}
		return nil
	}

	for _, iss := range allIssues {
		var idStr string
		switch iss.Status {
		case types.StatusOpen:
			idStr = ui.StatusOpenStyle.Render(iss.ID)
		case types.StatusInProgress:
			idStr = ui.StatusInProgressStyle.Render(iss.ID)
		case types.StatusBlocked:
			idStr = ui.StatusBlockedStyle.Render(iss.ID)
		case types.StatusClosed:
			idStr = ui.StatusClosedStyle.Render(iss.ID)
		default:
			idStr = iss.ID
		}
		fmt.Printf("  %s: %s [P%d] (%s) via %s\n",
			idStr, iss.Title, iss.Priority, iss.Status, iss.DependencyType)
	}
	fmt.Println()
	return nil
}

// runDepListRecordsProxiedServer answers `bd dep list a b c` with raw edge
// records grouped by source. It is off the Relations role deliberately: the
// role answers with the ISSUES on the far end of an anchor's edges, and this
// prints the edges themselves, per source, for several sources at once.
// Routing it through the role would mean N anchor probes and a hydration of
// every neighbor this output never shows.
func runDepListRecordsProxiedServer(ctx context.Context, args []string, typeFilter string) error {
	uw, err := proxiedOpenReadUOW(ctx)
	if err != nil {
		return err
	}
	defer uw.Close(ctx)

	depMap, err := uw.DependencyUseCase().GetIssueDependencyRecords(ctx, args)
	if err != nil {
		return HandleErrorRespectJSON("%v", err)
	}
	if jsonOutput {
		allDeps := []*types.Dependency{}
		for _, id := range args {
			for _, dep := range depMap[id] {
				if typeFilter == "" || string(dep.Type) == typeFilter {
					allDeps = append(allDeps, dep)
				}
			}
		}
		return outputJSON(allDeps)
	}
	for _, id := range args {
		deps := depMap[id]
		if len(deps) == 0 {
			fmt.Printf("\n%s has no dependencies\n", id)
			continue
		}
		fmt.Printf("\n%s %s depends on:\n\n", ui.RenderAccent("📋"), id)
		for _, dep := range deps {
			if typeFilter != "" && string(dep.Type) != typeFilter {
				continue
			}
			fmt.Printf("  %s via %s\n", dep.DependsOnID, dep.Type)
		}
	}
	fmt.Println()
	return nil
}

func runDepTreeProxiedServer(cmd *cobra.Command, ctx context.Context, args []string) error {
	fullID := args[0]
	showAllPaths, _ := cmd.Flags().GetBool("show-all-paths")
	maxDepth, _ := cmd.Flags().GetInt("max-depth")
	reverse, _ := cmd.Flags().GetBool("reverse")
	direction, _ := cmd.Flags().GetString("direction")
	statusFilter, _ := cmd.Flags().GetString("status")
	formatStr, _ := cmd.Flags().GetString("format")
	if strings.EqualFold(formatStr, "json") {
		jsonOutput = true
		formatStr = ""
	}
	if direction == "" && reverse {
		direction = "up"
	} else if direction == "" {
		direction = "down"
	}
	if direction != "down" && direction != "up" && direction != "both" {
		return HandleErrorRespectJSON("--direction must be 'down', 'up', or 'both'")
	}
	if maxDepth < 1 {
		return HandleErrorRespectJSON("--max-depth must be >= 1")
	}

	if uowProvider == nil {
		return HandleErrorRespectJSON("proxied-server UOW provider not initialized")
	}
	uw, err := uowProvider.NewUOW(ctx)
	if err != nil {
		return HandleErrorRespectJSON("open unit of work: %v", err)
	}
	defer uw.Close(ctx)

	depUC := uw.DependencyUseCase()
	var tree []*types.TreeNode

	if direction == "both" {
		downTree, err := depUC.GetDependencyTree(ctx, fullID, domain.DepTreeOpts{
			MaxDepth:     maxDepth,
			ShowAllPaths: showAllPaths,
			Direction:    domain.DepDirectionOut,
		})
		if err != nil {
			return HandleErrorRespectJSON("%v", err)
		}
		upTree, err := depUC.GetDependencyTree(ctx, fullID, domain.DepTreeOpts{
			MaxDepth:     maxDepth,
			ShowAllPaths: showAllPaths,
			Direction:    domain.DepDirectionIn,
		})
		if err != nil {
			return HandleErrorRespectJSON("%v", err)
		}
		tree = mergeBidirectionalTrees(downTree, upTree, fullID)
	} else {
		treeDir := domain.DepDirectionOut
		if direction == "up" {
			treeDir = domain.DepDirectionIn
		}
		var err error
		tree, err = depUC.GetDependencyTree(ctx, fullID, domain.DepTreeOpts{
			MaxDepth:     maxDepth,
			ShowAllPaths: showAllPaths,
			Direction:    treeDir,
		})
		if err != nil {
			return HandleErrorRespectJSON("%v", err)
		}
	}

	if statusFilter != "" {
		tree = filterTreeByStatus(tree, types.Status(statusFilter))
	}

	if formatStr == "mermaid" {
		outputMermaidTree(tree, args[0])
		return nil
	}

	if jsonOutput {
		if tree == nil {
			tree = []*types.TreeNode{}
		}
		_ = outputJSON(tree)
		return nil
	}

	if len(tree) == 0 {
		switch direction {
		case "up":
			fmt.Printf("\n%s has no dependents\n", fullID)
		case "both":
			fmt.Printf("\n%s has no dependencies or dependents\n", fullID)
		default:
			fmt.Printf("\n%s has no dependencies\n", fullID)
		}
		return nil
	}

	switch direction {
	case "up":
		fmt.Printf("\n%s Dependent tree for %s:\n\n", ui.RenderAccent("🌲"), fullID)
	case "both":
		fmt.Printf("\n%s Full dependency graph for %s:\n\n", ui.RenderAccent("🌲"), fullID)
	default:
		fmt.Printf("\n%s Dependency tree for %s:\n\n", ui.RenderAccent("🌲"), fullID)
	}

	renderTree(tree, maxDepth, direction)
	fmt.Println()
	return nil
}

func runDepCyclesProxiedServer(_ *cobra.Command, ctx context.Context) error {
	if uowProvider == nil {
		return HandleErrorRespectJSON("proxied-server UOW provider not initialized")
	}
	uw, err := uowProvider.NewUOW(ctx)
	if err != nil {
		return HandleErrorRespectJSON("open unit of work: %v", err)
	}
	defer uw.Close(ctx)

	cycles, err := uw.DependencyUseCase().DetectCycles(ctx)
	if err != nil {
		return HandleErrorRespectJSON("%v", err)
	}

	if jsonOutput {
		if cycles == nil {
			cycles = [][]*types.Issue{}
		}
		_ = outputJSON(cycles)
		return nil
	}

	if len(cycles) == 0 {
		fmt.Printf("\n%s No dependency cycles detected\n\n", ui.RenderPass("✓"))
		return nil
	}

	fmt.Printf("\n%s Found %d dependency cycles:\n\n", ui.RenderFail("⚠"), len(cycles))
	for i, cycle := range cycles {
		fmt.Printf("%d. Cycle involving:\n", i+1)
		for _, issue := range cycle {
			fmt.Printf("   - %s: %s\n", issue.ID, issue.Title)
		}
		fmt.Println()
	}
	return nil
}
