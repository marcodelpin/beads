package db

import (
	"database/sql"
	"fmt"
	"sort"

	"github.com/steveyegge/beads/internal/storage"
)

type idSrcPage struct {
	ordered  []idSrcRef
	issueIDs []string
	wispIDs  []string
}

func scanIDSrcPage(rows *sql.Rows, strictCrossTable bool) (idSrcPage, error) {
	defer func() { _ = rows.Close() }()

	var page idSrcPage
	seen := make(map[string]string)
	for rows.Next() {
		var id, src string
		if err := rows.Scan(&id, &src); err != nil {
			return idSrcPage{}, fmt.Errorf("scan: %w", err)
		}
		if prev, dup := seen[id]; dup {
			if strictCrossTable && prev != src {
				return idSrcPage{}, fmt.Errorf("id %q exists in both issues and wisps", id)
			}
			continue
		}
		seen[id] = src
		page.ordered = append(page.ordered, idSrcRef{id: id, src: src})
		switch src {
		case "i":
			page.issueIDs = append(page.issueIDs, id)
		case "w":
			page.wispIDs = append(page.wispIDs, id)
		}
	}
	if err := rows.Err(); err != nil {
		return idSrcPage{}, fmt.Errorf("rows: %w", err)
	}
	return page, nil
}

func orderByIDs[T any](ids []string, byID map[string]T) []T {
	out := make([]T, 0, len(ids))
	for _, id := range ids {
		if v, ok := byID[id]; ok {
			out = append(out, v)
		}
	}
	return out
}

func reassembleBySrc[T comparable](ordered []idSrcRef, issues, wisps map[string]T) []T {
	var zero T
	out := make([]T, 0, len(ordered))
	for _, p := range ordered {
		var v T
		switch p.src {
		case "i":
			v = issues[p.id]
		case "w":
			v = wisps[p.id]
		}
		if v != zero {
			out = append(out, v)
		}
	}
	return out
}

func (p *idSrcPage) trimToLimit(limit int) bool {
	if limit <= 0 || len(p.ordered) <= limit {
		return false
	}
	p.ordered = p.ordered[:limit]
	p.issueIDs = p.issueIDs[:0]
	p.wispIDs = p.wispIDs[:0]
	for _, r := range p.ordered {
		switch r.src {
		case "i":
			p.issueIDs = append(p.issueIDs, r.id)
		case "w":
			p.wispIDs = append(p.wispIDs, r.id)
		}
	}
	return true
}

func appendMetadataClauses(where []string, args []any, hasKey string, fields map[string]string) ([]string, []any, error) {
	if hasKey != "" {
		if err := storage.ValidateMetadataKey(hasKey); err != nil {
			return nil, nil, err
		}
		where = append(where, "JSON_EXTRACT(metadata, ?) IS NOT NULL")
		args = append(args, storage.JSONMetadataPath(hasKey))
	}
	if len(fields) > 0 {
		keys := make([]string, 0, len(fields))
		for k := range fields {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if err := storage.ValidateMetadataKey(k); err != nil {
				return nil, nil, err
			}
			where = append(where, "JSON_UNQUOTE(JSON_EXTRACT(metadata, ?)) = ?")
			args = append(args, storage.JSONMetadataPath(k), fields[k])
		}
	}
	return where, args, nil
}

type idSrcRef struct{ id, src string }

type sortDef struct {
	column     string
	defaultDir string
}

var sortDefs = map[string]sortDef{
	"":         {"priority", "ASC"},
	"priority": {"priority", "ASC"},
	"created":  {"created_at", "DESC"},
	"updated":  {"updated_at", "DESC"},
	"closed":   {"closed_at", "DESC"},
	"status":   {"status", "ASC"},
	"type":     {"issue_type", "ASC"},
	"assignee": {"assignee", "ASC"},
	"title":    {"title", "ASC"},
}

func sortColumnExpr(sortBy, prefix string) string {
	def := sortDefs[sortBy]
	qual := ""
	if prefix != "" {
		qual = prefix + "."
	}
	if sortBy == "title" {
		return fmt.Sprintf("LOWER(%stitle)", qual)
	}
	return qual + def.column
}

const unionSortColumnsSQL = `priority AS sort_priority,
	created_at AS sort_created,
	updated_at AS sort_updated,
	closed_at AS sort_closed,
	status AS sort_status,
	issue_type AS sort_type,
	assignee AS sort_assignee,
	LOWER(title) AS sort_title`

func unionOrderBySQL(sortBy string, sortDesc bool) string {
	if isGoSideSort(sortBy) {
		return ""
	}
	def, ok := sortDefs[sortBy]
	if !ok {
		def = sortDefs[""]
		sortBy = ""
	}
	dir := def.defaultDir
	if sortDesc {
		dir = flipDir(dir)
	}
	primary := "sort_priority"
	switch sortBy {
	case "", "priority":
		return fmt.Sprintf("ORDER BY sort_priority %s, sort_created DESC, id ASC", dir)
	case "created":
		primary = "sort_created"
	case "updated":
		primary = "sort_updated"
	case "closed":
		primary = "sort_closed"
	case "status":
		primary = "sort_status"
	case "type":
		primary = "sort_type"
	case "assignee":
		primary = "sort_assignee"
	case "title":
		primary = "sort_title"
	}
	return fmt.Sprintf("ORDER BY %s %s, id ASC", primary, dir)
}

func isGoSideSort(sortBy string) bool {
	return sortBy == "id"
}

func flipDir(dir string) string {
	if dir == "ASC" {
		return "DESC"
	}
	return "ASC"
}

func orderBySQL(sortBy string, sortDesc bool, prefix string) string {
	if isGoSideSort(sortBy) {
		return ""
	}
	def, ok := sortDefs[sortBy]
	if !ok {
		def = sortDefs[""]
		sortBy = ""
	}
	qual := ""
	if prefix != "" {
		qual = prefix + "."
	}
	dir := def.defaultDir
	if sortDesc {
		dir = flipDir(dir)
	}
	if sortBy == "" || sortBy == "priority" {
		return fmt.Sprintf("ORDER BY %spriority %s, %screated_at DESC, %sid ASC", qual, dir, qual, qual)
	}
	return fmt.Sprintf("ORDER BY %s %s, %sid ASC", sortColumnExpr(sortBy, prefix), dir, qual)
}

func limitOffsetSQL(limit, offset int) string {
	if limit <= 0 {
		return ""
	}
	if offset > 0 {
		return fmt.Sprintf("LIMIT %d OFFSET %d", limit+1, offset)
	}
	return fmt.Sprintf("LIMIT %d", limit+1)
}

func applyN1Overflow[T any](items []T, limit int) ([]T, bool) {
	if limit <= 0 || len(items) <= limit {
		return items, false
	}
	return items[:limit], true
}
