package main

import (
	"encoding/json"
	"fmt"
	"html/template"
	"strings"
)

// migrationDoc mirrors the tiny slice of pgroll's migration JSON shape that
// this demo's 7 scenario files actually use. Each operation is a
// single-key object (the key is the op name), so it's parsed generically
// into planOp and only the ops below are special-cased for rendering —
// this is a demo-scoped renderer, not a general pgroll-op viewer.
type migrationDoc struct {
	Operations []json.RawMessage `json:"operations"`
}

// planOp is one parsed operation: Name is the single JSON key
// ("add_column", "rename_column", ...), Body its value.
type planOp struct {
	Name string
	Body json.RawMessage
}

func parseMigrationPlan(migrationJSON string) ([]planOp, error) {
	var doc migrationDoc
	if err := json.Unmarshal([]byte(migrationJSON), &doc); err != nil {
		return nil, fmt.Errorf("parse migration json: %w", err)
	}
	ops := make([]planOp, 0, len(doc.Operations))
	for _, raw := range doc.Operations {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, fmt.Errorf("parse operation: %w", err)
		}
		for name, body := range m {
			ops = append(ops, planOp{Name: name, Body: body})
		}
	}
	return ops, nil
}

// ─── Diff view ──────────────────────────────────────────────────────────────

type addColumnBody struct {
	Table  string `json:"table"`
	Up     string `json:"up,omitempty"`
	Column struct {
		Name     string `json:"name"`
		Type     string `json:"type"`
		Nullable bool   `json:"nullable"`
		Default  string `json:"default,omitempty"`
	} `json:"column"`
}

type renameColumnBody struct {
	Table string `json:"table"`
	From  string `json:"from"`
	To    string `json:"to"`
}

type alterColumnBody struct {
	Table  string `json:"table"`
	Column string `json:"column"`
	Unique *struct {
		Name string `json:"name"`
	} `json:"unique,omitempty"`
}

// renderPlanDiff renders one diff-styled line per operation: additions in
// green, removals/renames-from in red. Scoped to the op shapes this demo
// actually emits (add_column, rename_column, alter_column/unique); anything
// else falls back to a neutral raw-op line.
func renderPlanDiff(ops []planOp) template.HTML {
	var b strings.Builder
	b.WriteString(`<div class="plan-diff">`)
	for _, op := range ops {
		switch op.Name {
		case "add_column":
			var body addColumnBody
			if err := json.Unmarshal(op.Body, &body); err == nil {
				null := "NOT NULL"
				if body.Column.Nullable {
					null = "NULL"
				}
				extra := ""
				if body.Column.Default != "" {
					extra += fmt.Sprintf(" DEFAULT %s", body.Column.Default)
				}
				if body.Up != "" {
					extra += fmt.Sprintf(" — backfilled from <code>%s</code>", template.HTMLEscapeString(body.Up))
				}
				fmt.Fprintf(&b, `<div class="diff-line add">+ %s.%s %s %s%s</div>`,
					template.HTMLEscapeString(body.Table), template.HTMLEscapeString(body.Column.Name),
					template.HTMLEscapeString(body.Column.Type), null, extra)
				continue
			}
		case "rename_column":
			var body renameColumnBody
			if err := json.Unmarshal(op.Body, &body); err == nil {
				fmt.Fprintf(&b, `<div class="diff-line del">- %s.%s</div><div class="diff-line add">+ %s.%s</div>`,
					template.HTMLEscapeString(body.Table), template.HTMLEscapeString(body.From),
					template.HTMLEscapeString(body.Table), template.HTMLEscapeString(body.To))
				continue
			}
		case "alter_column":
			var body alterColumnBody
			if err := json.Unmarshal(op.Body, &body); err == nil && body.Unique != nil {
				fmt.Fprintf(&b, `<div class="diff-line add">+ UNIQUE (%s.%s) — constraint %q</div>`,
					template.HTMLEscapeString(body.Table), template.HTMLEscapeString(body.Column),
					template.HTMLEscapeString(body.Unique.Name))
				continue
			}
		}
		fmt.Fprintf(&b, `<div class="diff-line neutral">~ %s</div>`, template.HTMLEscapeString(op.Name))
	}
	b.WriteString(`</div>`)
	return template.HTML(b.String())
}

// ─── Graph view ─────────────────────────────────────────────────────────────

// renderPlanGraph renders each operation as a node in a left-to-right flow,
// plus a fixed "applied" node representing the workflow committing the
// change. Node highlight state is driven purely by data-class expressions
// against signals already on the page ($phase/$percent) — no re-render is
// needed as the run progresses.
func renderPlanGraph(ops []planOp) template.HTML {
	var b strings.Builder
	b.WriteString(`<div class="plan-graph">`)
	for i, op := range ops {
		target := opTarget(op)
		fmt.Fprintf(&b, `<div class="graph-node" data-class="{done:$percent>=20,active:$percent>0&&$percent<20}">`+
			`<strong>%s</strong><span>%s</span></div>`,
			template.HTMLEscapeString(op.Name), template.HTMLEscapeString(target))
		if i < len(ops)-1 {
			b.WriteString(`<div class="graph-arrow">→</div>`)
		}
	}
	if len(ops) > 0 {
		b.WriteString(`<div class="graph-arrow">→</div>`)
	}
	b.WriteString(`<div class="graph-node" data-class="{done:$status=='completed',active:$phase=='waiting_for_app_ready'||$phase=='completing'||$phase=='verifying'}">` +
		`<strong>applied</strong><span>app-ready → complete</span></div>`)
	b.WriteString(`</div>`)
	return template.HTML(b.String())
}

func opTarget(op planOp) string {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(op.Body, &m); err != nil {
		return ""
	}
	if table, ok := m["table"]; ok {
		var t string
		if json.Unmarshal(table, &t) == nil {
			return t
		}
	}
	return ""
}
