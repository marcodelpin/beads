package main

import (
	"context"
	"encoding/csv"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/steveyegge/beads/internal/storage/domain"
	"github.com/steveyegge/beads/internal/storage/uow"
)

func runSQLProxiedServer(cmd *cobra.Command, ctx context.Context, query string, csvOutput bool) error {
	trimmed := strings.TrimSpace(strings.ToUpper(query))
	isRead := strings.HasPrefix(trimmed, "SELECT") ||
		strings.HasPrefix(trimmed, "EXPLAIN") ||
		strings.HasPrefix(trimmed, "PRAGMA") ||
		strings.HasPrefix(trimmed, "SHOW") ||
		strings.HasPrefix(trimmed, "DESCRIBE") ||
		strings.HasPrefix(trimmed, "WITH")

	if isRead {
		uw, err := openProxiedListUOW(ctx)
		if err != nil {
			return HandleErrorRespectJSON("%v", err)
		}
		defer uw.Close(ctx)

		result, err := uw.RawSQLUseCase().Query(ctx, query)
		if err != nil {
			return HandleErrorRespectJSON("query error: %v", err)
		}
		return renderRawSQLResult(result, csvOutput)
	}

	CheckReadonly("sql")

	var affected int64
	err := uow.RunInTxMsg(ctx, uowProvider, func(uw uow.UnitOfWork) (string, error) {
		n, err := uw.RawSQLUseCase().Exec(ctx, query)
		if err != nil {
			return "", err
		}
		affected = n
		return "bd sql: " + query, nil
	})
	if err != nil {
		return HandleErrorRespectJSON("exec error: %v", err)
	}

	if jsonOutput {
		return outputJSON(map[string]interface{}{
			"rows_affected": affected,
		})
	}

	fmt.Printf("OK, %d rows affected\n", affected)
	return nil
}

func renderRawSQLResult(result *domain.RawSQLResult, csvOutput bool) error {
	columns := result.Columns

	if jsonOutput {
		out := make([]map[string]interface{}, 0, len(result.Rows))
		for _, row := range result.Rows {
			m := make(map[string]interface{}, len(columns))
			for i, col := range columns {
				m[col] = row[i]
			}
			out = append(out, m)
		}
		return outputJSON(out)
	}

	if csvOutput {
		w := csv.NewWriter(os.Stdout)
		if err := w.Write(columns); err != nil {
			return HandleErrorRespectJSON("writing CSV header: %v", err)
		}
		for _, row := range result.Rows {
			record := make([]string, len(columns))
			for i := range columns {
				record[i] = fmt.Sprintf("%v", row[i])
			}
			if err := w.Write(record); err != nil {
				return HandleErrorRespectJSON("writing CSV row: %v", err)
			}
		}
		w.Flush()
		if err := w.Error(); err != nil {
			return HandleErrorRespectJSON("flushing CSV: %v", err)
		}
		return nil
	}

	if len(result.Rows) == 0 {
		fmt.Println("(0 rows)")
		return nil
	}

	widths := make([]int, len(columns))
	for i, col := range columns {
		widths[i] = len(col)
	}
	for _, row := range result.Rows {
		for i := range columns {
			s := fmt.Sprintf("%v", row[i])
			if len(s) > widths[i] {
				widths[i] = len(s)
			}
		}
	}
	for i := range widths {
		if widths[i] > 60 {
			widths[i] = 60
		}
	}

	for i, col := range columns {
		if i > 0 {
			fmt.Print(" | ")
		}
		fmt.Printf("%-*s", widths[i], col)
	}
	fmt.Println()

	for i := range columns {
		if i > 0 {
			fmt.Print("-+-")
		}
		fmt.Print(strings.Repeat("-", widths[i]))
	}
	fmt.Println()

	for _, row := range result.Rows {
		for i := range columns {
			if i > 0 {
				fmt.Print(" | ")
			}
			s := fmt.Sprintf("%v", row[i])
			if len(s) > 60 {
				s = s[:57] + "..."
			}
			fmt.Printf("%-*s", widths[i], s)
		}
		fmt.Println()
	}

	fmt.Printf("(%d rows)\n", len(result.Rows))
	return nil
}
