package knowledge

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/yichenfchai/AegisFlow/backend/internal/auth"
	"github.com/yichenfchai/AegisFlow/backend/internal/dbmigrate"
	"github.com/yichenfchai/AegisFlow/backend/internal/testutil"
)

var (
	adminActor  = auth.Actor{ID: "admin-0000000000000001", Username: "root", Role: "admin", RequestID: "req-admin"}
	memberActor = auth.Actor{ID: "member-000000000000001", Username: "mia", Role: "member", RequestID: "req-member"}
	viewerActor = auth.Actor{ID: "viewer-000000000000001", Username: "vic", Role: "viewer", RequestID: "req-viewer"}
)

func testService(t *testing.T) (*Service, *sql.DB) {
	t.Helper()
	db := testutil.OpenPostgres(t)
	if err := dbmigrate.Apply(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	for _, actor := range []auth.Actor{adminActor, memberActor, viewerActor} {
		if _, err := db.Exec(`INSERT INTO auth_users(id,username,password_hash,display_name,role)
		 VALUES($1,$2,'unused',$2,$3)`, actor.ID, actor.Username, actor.Role); err != nil {
			t.Fatal(err)
		}
	}
	return New(db), db
}

type auditRow struct {
	Action string
	Detail string
}

func auditRows(t *testing.T, db *sql.DB, objectID string) []auditRow {
	t.Helper()
	rows, err := db.Query(`SELECT action,detail FROM auth_audit
		WHERE object_type='knowledge_doc' AND object_id=$1 ORDER BY created_at,id`, objectID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var audit []auditRow
	for rows.Next() {
		var row auditRow
		if err := rows.Scan(&row.Action, &row.Detail); err != nil {
			t.Fatal(err)
		}
		audit = append(audit, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return audit
}

func createDoc(t *testing.T, s *Service, title, body, tags string) *Document {
	t.Helper()
	doc, err := s.Create(context.Background(), adminActor, Input{Title: title, Body: body, Tags: tags})
	if err != nil {
		t.Fatalf("create %q: %v", title, err)
	}
	return doc
}

func TestCreateGetUpdateDeleteAndAudit(t *testing.T) {
	s, db := testService(t)
	ctx := context.Background()
	body := "# 发布手册\n\n步骤：**先备份**，再执行 `rollback`。\n不执行 <script>alert(1)</script>。"
	doc := createDoc(t, s, "  发布手册  ", body, "  ops, runbook ,,  security ")
	if doc.ID == "" || doc.Title != "发布手册" || doc.Tags != "ops,runbook,security" {
		t.Fatalf("created doc = %+v", doc)
	}
	if doc.Body != body {
		t.Fatalf("body not stored verbatim: %q", doc.Body)
	}
	if doc.CreatedByName != "root" || doc.UpdatedByName != "root" || doc.CreatedAt == 0 || doc.UpdatedAt != doc.CreatedAt {
		t.Fatalf("created metadata = %+v", doc)
	}

	fetched, err := s.Get(ctx, doc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fetched.Body != body || fetched.Title != "发布手册" {
		t.Fatalf("fetched = %+v", fetched)
	}

	list, total, err := s.List(ctx, ListFilter{Limit: 20, Offset: 0})
	if err != nil || total != 1 || len(list) != 1 {
		t.Fatalf("list: %v total=%d len=%d", err, total, len(list))
	}
	if list[0].ID != doc.ID || list[0].Title != "发布手册" {
		t.Fatalf("list item = %+v", list[0])
	}
	// The list projection has no body field at all.
	encoded, err := json.Marshal(list)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "body") || strings.Contains(string(encoded), "script") {
		t.Fatalf("list leaked a body: %s", encoded)
	}

	// Update replaces the body/tags, stamps the updater and keeps the creator.
	updated, err := s.Update(ctx, adminActor, doc.ID, Input{Title: "发布手册 v2", Body: "new **body**", Tags: "ops"})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Title != "发布手册 v2" || updated.Body != "new **body**" || updated.Tags != "ops" ||
		updated.CreatedBy != adminActor.ID || updated.UpdatedAt < updated.CreatedAt {
		t.Fatalf("updated doc = %+v", updated)
	}

	// Three audit rows, each with id and title only; the body never appears.
	audit := auditRows(t, db, doc.ID)
	if len(audit) != 2 {
		t.Fatalf("audit rows = %+v", audit)
	}
	byAction := make(map[string]string, len(audit))
	for _, row := range audit {
		byAction[row.Action] = row.Detail
	}
	for _, action := range []string{ActionCreate, ActionUpdate} {
		detail, ok := byAction[action]
		if !ok || !strings.Contains(detail, doc.ID) || !strings.Contains(detail, "title=") {
			t.Fatalf("%s audit detail = %q", action, detail)
		}
		if strings.Contains(detail, "rollback") || strings.Contains(detail, "script") || strings.Contains(detail, "new **body**") {
			t.Fatalf("%s audit detail leaked the body: %q", action, detail)
		}
	}
	if !strings.Contains(byAction[ActionUpdate], "发布手册 v2") {
		t.Fatalf("update audit detail = %q", byAction[ActionUpdate])
	}

	if err = s.Delete(ctx, adminActor, doc.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Get(ctx, doc.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get after delete: %v", err)
	}
	audit = auditRows(t, db, doc.ID)
	if len(audit) != 3 {
		t.Fatalf("delete audit = %+v", audit)
	}
	deleteDetail := ""
	for _, row := range audit {
		if row.Action == ActionDelete {
			deleteDetail = row.Detail
		}
	}
	if !strings.Contains(deleteDetail, "发布手册 v2") || !strings.Contains(deleteDetail, doc.ID) {
		t.Fatalf("delete audit detail = %q", deleteDetail)
	}
}

func TestPermissionsAndValidation(t *testing.T) {
	s, _ := testService(t)
	ctx := context.Background()
	doc := createDoc(t, s, "unprivileged", "member cannot write", "")

	// Reads are open to every role.
	if _, err := s.Get(ctx, doc.ID); err != nil {
		t.Fatalf("member get: %v", err)
	}
	if _, _, err := s.List(ctx, ListFilter{Limit: 20}); err != nil {
		t.Fatalf("viewer list: %v", err)
	}
	// Writes are admin-only in the service itself, not just the HTTP routes.
	for _, actor := range []auth.Actor{memberActor, viewerActor} {
		if _, err := s.Create(ctx, actor, Input{Title: "nope"}); !errors.Is(err, ErrForbidden) {
			t.Fatalf("create as %s: %v", actor.Role, err)
		}
		if _, err := s.Update(ctx, actor, doc.ID, Input{Title: "nope"}); !errors.Is(err, ErrForbidden) {
			t.Fatalf("update as %s: %v", actor.Role, err)
		}
		if err := s.Delete(ctx, actor, doc.ID); !errors.Is(err, ErrForbidden) {
			t.Fatalf("delete as %s: %v", actor.Role, err)
		}
	}

	// Payload bounds.
	long := strings.Repeat("a", maxTitleBytes+1)
	cases := []struct {
		name  string
		input Input
	}{
		{"empty title", Input{Title: "   "}},
		{"long title", Input{Title: long}},
		{"multiline title", Input{Title: "a\nb"}},
		{"invalid body utf8", Input{Title: "t", Body: string([]byte{0xff})}},
		{"nul body", Input{Title: "t", Body: "a\x00b"}},
		{"too large body", Input{Title: "t", Body: strings.Repeat("b", maxBodyBytes+1)}},
		{"multiline tags", Input{Title: "t", Tags: "a\nb"}},
		{"too large tags", Input{Title: "t", Tags: strings.Repeat("c", maxTagsBytes+1)}},
	}
	for _, tc := range cases {
		if _, err := s.Create(ctx, adminActor, tc.input); !errors.Is(err, ErrValidation) {
			t.Fatalf("%s: %v", tc.name, err)
		}
	}
	// Boundary values are accepted: a 200-byte title and a 100 KiB body.
	maxDoc, err := s.Create(ctx, adminActor, Input{Title: strings.Repeat("t", maxTitleBytes), Body: strings.Repeat("x", maxBodyBytes)})
	if err != nil {
		t.Fatalf("max-size document: %v", err)
	}
	if len(maxDoc.Body) != maxBodyBytes {
		t.Fatalf("stored body bytes = %d", len(maxDoc.Body))
	}

	// Filters and ids.
	if _, _, err := s.List(ctx, ListFilter{Query: strings.Repeat("q", maxQueryBytes+1), Limit: 20}); !errors.Is(err, ErrValidation) {
		t.Fatalf("long q: %v", err)
	}
	for _, filter := range []ListFilter{{Limit: 0}, {Limit: 101}, {Limit: 20, Offset: -1}} {
		if _, _, err := s.List(ctx, filter); !errors.Is(err, ErrValidation) {
			t.Fatalf("filter %+v: %v", filter, err)
		}
	}
	if _, err := s.Get(ctx, "short"); !errors.Is(err, ErrValidation) {
		t.Fatalf("invalid id: %v", err)
	}
	if _, err := s.Get(ctx, "missing-0000000000001"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id: %v", err)
	}
	if _, err := s.Update(ctx, adminActor, "missing-0000000000001", Input{Title: "x"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update unknown: %v", err)
	}
	if err := s.Delete(ctx, adminActor, "missing-0000000000001"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete unknown: %v", err)
	}
}

func TestSearchEscapingOrderingAndPagination(t *testing.T) {
	s, db := testService(t)
	ctx := context.Background()
	percent := createDoc(t, s, "100% rollback", "literal percent", "")
	plain := createDoc(t, s, "100x rollback", "no percent", "")
	underscore := createDoc(t, s, "a_b plan", "literal underscore", "")
	_ = plain
	// Force a deterministic updated_at ordering independent of the test clock.
	if _, err := db.Exec(`UPDATE knowledge_docs SET updated_at=1000 WHERE id=$1`, percent.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE knowledge_docs SET updated_at=2000 WHERE id=$1`, underscore.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE knowledge_docs SET updated_at=3000 WHERE id=$1`, plain.ID); err != nil {
		t.Fatal(err)
	}

	// '%' and '_' are literal: 100% must not match 100x.
	list, total, err := s.List(ctx, ListFilter{Query: "100%", Limit: 20})
	if err != nil || total != 1 || list[0].ID != percent.ID {
		t.Fatalf("100%% search: %v total=%d list=%+v", err, total, list)
	}
	if list, total, err = s.List(ctx, ListFilter{Query: "a_b", Limit: 20}); err != nil || total != 1 || list[0].ID != underscore.ID {
		t.Fatalf("underscore search: %v total=%d list=%+v", err, total, list)
	}
	if _, total, err = s.List(ctx, ListFilter{Query: "100", Limit: 20}); err != nil || total != 2 {
		t.Fatalf("plain substring: %v total=%d", err, total)
	}
	if _, total, err = s.List(ctx, ListFilter{Query: "%", Limit: 20}); err != nil || total != 1 {
		t.Fatalf("literal percent only: %v total=%d", err, total)
	}
	if _, total, err = s.List(ctx, ListFilter{Query: "   ", Limit: 20}); err != nil || total != 3 {
		t.Fatalf("blank query matches all: %v total=%d", err, total)
	}
	// Ordering: newest update first.
	list, _, err = s.List(ctx, ListFilter{Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if list[0].ID != plain.ID || list[1].ID != underscore.ID || list[2].ID != percent.ID {
		t.Fatalf("ordering = %+v", list)
	}
	// Pagination shares total and limit/offset between count and page.
	page, total, err := s.List(ctx, ListFilter{Limit: 1, Offset: 1})
	if err != nil || total != 3 || len(page) != 1 || page[0].ID != underscore.ID {
		t.Fatalf("page: %v total=%d page=%+v", err, total, page)
	}
}
